package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeviceFlow signs a user in through the browser without a password or a
// pasted token: GitHub's OAuth device flow. The app shows a short code, the
// user confirms it on github.com, and GitHub hands the app a token.
//
// It needs the Client ID of an OAuth App that has "Enable Device Flow"
// switched on; no client secret is involved.
type DeviceFlow struct {
	ClientID string
	// Scopes requested for the token; DefaultScopes when empty.
	Scopes []string
	// BaseURL is https://github.com unless overridden (tests).
	BaseURL string
	HTTP    *http.Client
	// Sleep waits between polls; nil means a real pause. Tests replace it.
	Sleep func(context.Context, time.Duration) error
}

// DefaultScopes is what the app needs: write code and write workflow files.
var DefaultScopes = []string{"repo", "workflow"}

// DeviceCode is what the user has to confirm.
type DeviceCode struct {
	// UserCode is the short code to type, like "WDJB-MJHT".
	UserCode string
	// VerificationURL is the page where it is typed.
	VerificationURL string
	// Expires is when the code stops working.
	Expires time.Time

	deviceCode string
	interval   time.Duration
}

// Device-flow failures the UI words differently.
var (
	ErrDeviceDenied   = errors.New("вход отменён на странице GitHub")
	ErrDeviceExpired  = errors.New("код устарел — запросите новый")
	ErrDeviceDisabled = errors.New("у OAuth-приложения выключен Device Flow: включите «Enable Device Flow» в его настройках на GitHub")
	ErrDeviceClient   = errors.New("GitHub не знает такой Client ID: проверьте его в настройках")
)

func (f *DeviceFlow) base() string {
	if f.BaseURL != "" {
		return strings.TrimRight(f.BaseURL, "/")
	}
	return "https://github.com"
}

func (f *DeviceFlow) post(ctx context.Context, path string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kolibri-forge")
	client := f.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, friendly(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, friendly(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, ErrDeviceClient
		}
		return resp.StatusCode, fmt.Errorf("GitHub ответил не так, как ожидалось (HTTP %d)", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

type deviceError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (e deviceError) asError() error {
	switch e.Error {
	case "":
		return nil
	case "access_denied":
		return ErrDeviceDenied
	case "expired_token", "token_expired":
		return ErrDeviceExpired
	case "device_flow_disabled":
		return ErrDeviceDisabled
	case "incorrect_client_credentials", "unauthorized_client", "Not Found":
		return ErrDeviceClient
	}
	if e.Description != "" {
		return fmt.Errorf("GitHub отказал во входе: %s", e.Description)
	}
	return fmt.Errorf("GitHub отказал во входе: %s", e.Error)
}

// Start asks GitHub for a code to show the user.
func (f *DeviceFlow) Start(ctx context.Context) (DeviceCode, error) {
	if strings.TrimSpace(f.ClientID) == "" {
		return DeviceCode{}, errors.New("не указан Client ID OAuth-приложения")
	}
	scopes := f.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	var answer struct {
		deviceError
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	form := url.Values{"client_id": {strings.TrimSpace(f.ClientID)}, "scope": {strings.Join(scopes, " ")}}
	status, err := f.post(ctx, "/login/device/code", form, &answer)
	if err != nil {
		return DeviceCode{}, err
	}
	if err := answer.asError(); err != nil {
		return DeviceCode{}, err
	}
	if answer.DeviceCode == "" || answer.UserCode == "" {
		if status == http.StatusNotFound {
			return DeviceCode{}, ErrDeviceClient
		}
		return DeviceCode{}, fmt.Errorf("GitHub не выдал код входа (HTTP %d)", status)
	}
	code := DeviceCode{
		UserCode:        answer.UserCode,
		VerificationURL: answer.VerificationURI,
		Expires:         time.Now().Add(time.Duration(answer.ExpiresIn) * time.Second),
		deviceCode:      answer.DeviceCode,
		interval:        time.Duration(answer.Interval) * time.Second,
	}
	if code.VerificationURL == "" {
		code.VerificationURL = "https://github.com/login/device"
	}
	if answer.ExpiresIn <= 0 {
		code.Expires = time.Now().Add(15 * time.Minute)
	}
	if code.interval <= 0 {
		code.interval = 5 * time.Second
	}
	return code, nil
}

// Wait blocks until the user confirms the code and returns the token, or
// fails when they refuse, the code expires or ctx is cancelled.
func (f *DeviceFlow) Wait(ctx context.Context, code DeviceCode) (string, error) {
	sleep := f.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	interval := code.interval
	form := url.Values{
		"client_id":   {strings.TrimSpace(f.ClientID)},
		"device_code": {code.deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	for {
		if err := sleep(ctx, interval); err != nil {
			return "", err
		}
		if !code.Expires.IsZero() && time.Now().After(code.Expires) {
			return "", ErrDeviceExpired
		}
		var answer struct {
			deviceError
			AccessToken string `json:"access_token"`
			Interval    int    `json:"interval"`
		}
		if _, err := f.post(ctx, "/login/oauth/access_token", form, &answer); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// A dropped connection is not a refusal: keep polling until
			// the code itself expires.
			var net *netError
			if errors.As(err, &net) {
				continue
			}
			return "", err
		}
		switch answer.Error {
		case "":
			if answer.AccessToken == "" {
				return "", errors.New("GitHub подтвердил вход, но не прислал токен")
			}
			return answer.AccessToken, nil
		case "authorization_pending":
			// The user has not confirmed yet.
		case "slow_down":
			// GitHub asks for a longer pause and says how long.
			if answer.Interval > 0 {
				interval = time.Duration(answer.Interval) * time.Second
			} else {
				interval += 5 * time.Second
			}
		default:
			return "", answer.asError()
		}
	}
}
