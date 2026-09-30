package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func NativeDialog(ctx context.Context) (string, bool, error) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "osascript", "-e", `set selectedFile to choose file with prompt "Choose a SQLite database to edit directly"`, "-e", `POSIX path of selectedFile`)
	case "linux":
		command = exec.CommandContext(ctx, "zenity", "--file-selection", "--title=Choose a SQLite database to edit directly")
	case "windows":
		command = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-Command", `Add-Type -AssemblyName System.Windows.Forms; $dialog = New-Object System.Windows.Forms.OpenFileDialog; $dialog.Title = 'Choose a SQLite database to edit directly'; if ($dialog.ShowDialog() -eq 'OK') { [Console]::Write($dialog.FileName) }`)
	default:
		return "", false, errors.New("native file picker is unavailable on this operating system")
	}
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			message := strings.ToLower(string(exit.Stderr))
			if strings.Contains(message, "cancel") || strings.Contains(message, "-128") || runtime.GOOS == "linux" && exit.ExitCode() == 1 {
				return "", true, nil
			}
			return "", false, fmt.Errorf("native file picker failed: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return "", false, fmt.Errorf("native file picker failed: %w", err)
	}
	path := strings.TrimRight(string(output), "\r\n")
	return path, path == "", nil
}
