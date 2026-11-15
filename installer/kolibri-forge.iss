; Установщик Kolibri Forge для Windows (Inno Setup 6).
;
; Собирается в GitHub Actions, см. .github/workflows/build.yml:
;   ISCC /DAppVersion=1.0.0 /DAppBuild=42 /DSourceExe="C:\path\Kolibri Forge.exe" /Odist installer\kolibri-forge.iss
;
; Файл сохранён в UTF-8 с BOM: без него Inno Setup прочитает русский текст
; в кодировке системы.

#ifndef AppVersion
  #define AppVersion "1.0.0"
#endif
#ifndef AppBuild
  #define AppBuild "0"
#endif
#ifndef SourceExe
  #define SourceExe "..\Kolibri Forge.exe"
#endif

#define AppName "Kolibri Forge"
#define AppExe "Kolibri Forge.exe"
#define AppURL "https://github.com/zigorminsk-debug/kolibri"

[Setup]
; AppId связывает обновление с уже установленной версией — не менять.
AppId={{041561ED-4A7B-412A-BE6A-16764A792822}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher=zigorminsk-debug
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}/issues
AppUpdatesURL={#AppURL}/releases
VersionInfoVersion={#AppVersion}.{#AppBuild}
VersionInfoProductName={#AppName}
VersionInfoDescription=Установка {#AppName}

; Ставится без прав администратора, в профиль пользователя; в окне выбора
; можно установить для всех — тогда Windows запросит права.
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
DefaultDirName={autopf}\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
DisableDirPage=auto
UsePreviousAppDir=yes

; Программа собрана для 64-битной Windows 10 и новее.
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0

WizardStyle=modern
SetupIconFile=icon.ico
UninstallDisplayIcon={app}\{#AppExe}
UninstallDisplayName={#AppName}
Compression=lzma2/max
SolidCompression=yes
OutputBaseFilename=KolibriForge-Setup
; При обновлении установщик предложит закрыть запущенную программу.
CloseApplications=yes
RestartApplications=no

[Languages]
Name: "ru"; MessagesFile: "compiler:Languages\Russian.isl"
Name: "en"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
ru.KeepSettings=Удалить также настройки и журнал Kolibri Forge?%n%nВ настройках хранится токен доступа к GitHub. Ответьте «Нет», если собираетесь установить программу снова.
en.KeepSettings=Also remove the settings and the log of Kolibri Forge?%n%nThe settings hold your GitHub access token. Answer "No" if you plan to install the program again.

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
Source: "{#SourceExe}"; DestDir: "{app}"; DestName: "{#AppExe}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\{#AppName}"; Filename: "{app}\{#AppExe}"
Name: "{autodesktop}\{#AppName}"; Filename: "{app}\{#AppExe}"; Tasks: desktopicon

[Run]
Filename: "{app}\{#AppExe}"; Description: "{cm:LaunchProgram,{#AppName}}"; Flags: nowait postinstall skipifsilent

[Code]
// Настройки и журнал лежат в %AppData%\kolibri-forge и переживают удаление
// программы, если пользователь не попросит стереть и их. При тихом
// удалении вопрос не задаётся и настройки остаются.
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Dir: String;
begin
  if CurUninstallStep <> usPostUninstall then
    exit;
  Dir := ExpandConstant('{userappdata}\kolibri-forge');
  if not DirExists(Dir) then
    exit;
  if SuppressibleMsgBox(CustomMessage('KeepSettings'), mbConfirmation, MB_YESNO or MB_DEFBUTTON2, IDNO) = IDYES then
    DelTree(Dir, True, True, True);
end;
