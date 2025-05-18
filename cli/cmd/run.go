package cmd

import (
	"ags/lib"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"
)

var (
	targetDir string
	logFile   string
	args      []string
	watch     bool

	activeGjsCmd *exec.Cmd
)

var runCommand = &cobra.Command{
	Use:     "run [file]",
	Short:   "Run an app",
	Example: "  run app.ts --define DEBUG=true",
	Args:    cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) > 0 {
			path, err := filepath.Abs(args[0])
			if err != nil {
				lib.Err(err)
			}

			info, err := os.Stat(path)
			if err != nil {
				lib.Err(err)
			}

			if info.IsDir() {
				run(getAppEntry(path), "")
			} else {
				run(path, "")
			}

		} else {
			run(getAppEntry(targetDir), targetDir)
		}
	},
}

func init() {
	f := runCommand.Flags()
	f.StringVarP(&targetDir, "directory", "d", defaultConfigDir(),
		`directory to search for an "app" entry file
when no positional argument is given
`+"\b")

	f.StringSliceVar(&defines, "define", []string{}, "replace global identifiers with constant expressions")
	f.StringArrayVar(&alias, "alias", []string{}, "alias packages")
	f.StringArrayVarP(&args, "arg", "a", []string{}, "cli args to pass to gjs")
	f.UintVarP(&gtkVersion, "gtk", "g", 0, "gtk version")
	f.StringVar(&logFile, "log-file", "", "file to redirect the stdout of gjs to")
	f.BoolVarP(&watch, "watch", "w", false, "watch for changes and restart the app")
	f.MarkHidden("package")
	f.MarkHidden("alias")
}

func getOutfile() string {
	rundir, found := os.LookupEnv("XDG_RUNTIME_DIR")

	if !found {
		rundir = "/tmp"
	}

	return filepath.Join(rundir, "ags.js")
}

func getAppEntry(dir string) string {
	path, err := filepath.Abs(dir)
	if err != nil {
		lib.Err(err)
	}

	infile := filepath.Join(path, "app")
	exts := []string{"js", "ts", "jsx", "tsx"}

	i := slices.IndexFunc(exts, func(ext string) bool {
		_, err := os.Stat(infile + "." + ext)
		return !os.IsNotExist(err)
	})

	if i == -1 {
		msg := "no such file or directory: " +
			fmt.Sprintf("\"%s\"\n", lib.Cyan(dir+"/app")) +
			lib.Cyan("tip: ") + "valid names are: "
		for _, v := range exts {
			msg = msg + fmt.Sprintf(` "%s"`, lib.Cyan("app."+v))
		}
		lib.Err(msg)
	}

	return infile + "." + exts[i]
}

func logging() (io.Writer, io.Writer, *os.File) {
	if logFile == "" {
		return os.Stdout, os.Stderr, nil
	}

	lib.Mkdir(filepath.Dir(logFile))
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		lib.Err(err)
	}

	return io.MultiWriter(os.Stdout, file), io.MultiWriter(os.Stderr, file), file
}

// prepareGjsCommand prepares a new gjs command.
// If an existing 'currentGjsCmdToKill' is provided and is not nil, it attempts to kill it.
// 'cliArgs' are the user-provided arguments intended for the gjs process.
func prepareGjsCommand(
	currentGjsCmdToKill *exec.Cmd,
	infile string,
	outfile string,
	stdout io.Writer,
	stderr io.Writer,
	isGtk4 bool,
	gtk4LayerShellPath string,
	cliArgs []string,
) *exec.Cmd {
	if currentGjsCmdToKill != nil && currentGjsCmdToKill.Process != nil {
		if err := currentGjsCmdToKill.Process.Kill(); err != nil {
			// Log error instead of exiting, especially useful in watch mode
			fmt.Fprintf(os.Stderr, "Error killing previous gjs process: %v\n", err)
		}
		// Wait for the process to exit and release resources.
		// Consider adding a timeout if Wait() could hang indefinitely.
		currentGjsCmdToKill.Wait()
	}

	if isGtk4 {
		os.Setenv("LD_PRELOAD", gtk4LayerShellPath)
	}

	// Arguments for the gjs executable: run as module, the output file, then user's CLI args
	gjsExecutableArgs := append([]string{"-m", outfile}, cliArgs...)

	newCmd := lib.Exec(gjs, gjsExecutableArgs...)
	newCmd.Stdin = os.Stdin
	newCmd.Dir = filepath.Dir(infile) // Set working directory to the input file's directory
	newCmd.Stdout = stdout
	newCmd.Stderr = stderr

	return newCmd
}

func run(infile string, rootdir string) {
	if gtkVersion == 0 {
		gtkVersion = inferGtkVersion(infile)
	}
	isGtk4 := (gtkVersion == 4) // Determine GTK version status once

	outfile := getOutfile()

	// Define bundle options once
	bundleOptions := lib.BundleOpts{
		Infile:           infile,
		Outfile:          outfile,
		Defines:          defines,
		Alias:            alias,
		GtkVersion:       gtkVersion,
		WorkingDirectory: rootdir,
	}

	// Perform initial bundle
	lib.Bundle(bundleOptions)

	stdout, stderr, logFileHandle := logging() // Get configured log writers

	// Note: The global 'args' slice (populated by Cobra flags) is passed directly.
	// 'prepareGjsCommand' now handles prepending "-m" and the outfile.

	if watch {
		// For watch mode, 'activeGjsCmd' tracks the currently running process.
		// Initial run:
		activeGjsCmd = prepareGjsCommand(nil, infile, outfile, stdout, stderr, isGtk4, gtk4LayerShell, args)
		if err := activeGjsCmd.Start(); err != nil {
			if logFileHandle != nil {
				logFileHandle.Close()
			}
			lib.Err(fmt.Errorf("failed to start gjs initially in watch mode: %w", err))
		}

		lib.Watch(lib.WatchOpts{
			BundleOpts: bundleOptions, // Pass the bundle options for re-bundling
			ReloadPluginOpts: lib.ReloadPluginOpts{
				OnBuild: func() {
					// On subsequent builds, 'activeGjsCmd' holds the command to be killed.
					// The new command will be assigned back to 'activeGjsCmd'.
					activeGjsCmd = prepareGjsCommand(activeGjsCmd, infile, outfile, stdout, stderr, isGtk4, gtk4LayerShell, args)
					if err := activeGjsCmd.Start(); err != nil {
						fmt.Fprintf(os.Stderr, "Failed to restart gjs: %v\n", err)
						// Decide if watch should continue or exit on failed restart
					}
				},
				OnExit: func() {
					if logFileHandle != nil {
						logFileHandle.Close()
					}
					// Ensure the last gjs process is killed if the watcher exits.
					if activeGjsCmd != nil && activeGjsCmd.Process != nil {
						activeGjsCmd.Process.Kill()
						activeGjsCmd.Wait() // Wait for cleanup
					}
				},
			},
		})
	} else {
		// Non-watch mode: create command, run it synchronously, and clean up.
		// No old process to kill, so pass nil for currentGjsCmdToKill.
		cmd := prepareGjsCommand(nil, infile, outfile, stdout, stderr, isGtk4, gtk4LayerShell, args)
		if err := cmd.Run(); err != nil {
			// lib.Err will typically print the error and exit.
			// Ensure logFileHandle is closed even on error before lib.Err exits.
			if logFileHandle != nil {
				logFileHandle.Close()
			}
			lib.Err(err) 
		}

		if logFileHandle != nil {
			logFileHandle.Close()
		}
	}
}