package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/xxxsen/common/idgen"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/backupmgr"
	"github.com/xxxsen/tgfile/config"
	"github.com/xxxsen/tgfile/db"
)

func newBackupCommand(ctx context.Context) *cobra.Command {
	command := &cobra.Command{
		Use:   "backup",
		Short: "Export, verify, or import a logical backup",
		Args:  noPositionalArgs,
		RunE: func(*cobra.Command, []string) error {
			return usageError("a backup subcommand is required")
		},
	}
	command.AddCommand(
		newBackupExportCommand(ctx),
		newBackupVerifyCommand(ctx),
		newBackupImportCommand(ctx),
	)
	return command
}

func newBackupExportCommand(ctx context.Context) *cobra.Command {
	var configFile, scope, output string
	command := &cobra.Command{
		Use:   "export",
		Short: "Create a verified logical backup artifact",
		Args:  noPositionalArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if output == "" || output == "-" {
				return usageError("--output must be a regular file path")
			}
			manager, closeRuntime, err := openBackupRuntime(ctx, configFile)
			if err != nil {
				return err
			}
			defer closeRuntime()
			job, err := manager.CreateExport(ctx, backupmgr.CreateExportRequest{
				Owner: "cli", IdempotencyKey: cliIdempotencyKey("export"), Scope: scope,
			})
			if err != nil {
				return fmt.Errorf("create backup export: %w", err)
			}
			job, err = manager.ProcessUntilTerminal(ctx, job.JobID)
			if err != nil {
				return err
			}
			artifact, _, err := manager.Artifact(ctx, job.JobID)
			if err != nil {
				return err
			}
			if err := copyBackupArtifact(artifact, output); err != nil {
				return err
			}
			return writeCommandJSON(command, job)
		},
	}
	command.Flags().StringVar(&configFile, "config", "./config.json", "config file path")
	command.Flags().StringVar(&scope, "scope", "/", "absolute mapping path to export")
	command.Flags().StringVar(&output, "output", "", "output .tgfb file")
	return command
}

func newBackupVerifyCommand(ctx context.Context) *cobra.Command {
	var configFile, input string
	command := &cobra.Command{
		Use:   "verify",
		Short: "Verify a logical backup without opening the database or backend",
		Args:  noPositionalArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if input == "" || input == "-" {
				return usageError("--input must be a regular file path")
			}
			serviceConfig, err := config.Parse(configFile)
			if err != nil {
				return fmt.Errorf("parse config: %w", err)
			}
			if err := serviceConfig.Validate(); err != nil {
				return fmt.Errorf("validate config: %w", err)
			}
			limits := toBackupManagerOptions(serviceConfig, math.MaxInt64).Limits
			_, report, err := backupfmt.VerifyFile(ctx, input, limits, math.MaxInt64)
			if err != nil {
				return fmt.Errorf("verify backup: %w", err)
			}
			return writeCommandJSON(command, report)
		},
	}
	command.Flags().StringVar(&configFile, "config", "./config.json", "config file path")
	command.Flags().StringVar(&input, "input", "", "input .tgfb file")
	return command
}

func newBackupImportCommand(ctx context.Context) *cobra.Command {
	var configFile, input, conflict string
	var dryRun bool
	command := &cobra.Command{
		Use:   "import",
		Short: "Verify and restore a logical backup",
		Args:  noPositionalArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if input == "" || input == "-" {
				return usageError("--input must be a regular file path")
			}
			file, err := os.Open(input)
			if err != nil {
				return fmt.Errorf("open backup input: %w", err)
			}
			defer func() { _ = file.Close() }()
			stat, err := file.Stat()
			if err != nil {
				return fmt.Errorf("stat backup input: %w", err)
			}
			checksum, err := backupFileSHA256(ctx, file)
			if err != nil {
				return err
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewind backup input: %w", err)
			}
			manager, closeRuntime, err := openBackupRuntime(ctx, configFile)
			if err != nil {
				return err
			}
			defer closeRuntime()
			job, err := manager.CreateImport(ctx, backupmgr.CreateImportRequest{
				Owner: "cli", IdempotencyKey: cliIdempotencyKey("import"),
				ConflictPolicy: conflict, DryRun: dryRun, ContentLength: stat.Size(),
				ArtifactSHA256: checksum, Body: file,
			})
			if err != nil {
				return fmt.Errorf("create backup import: %w", err)
			}
			job, err = manager.ProcessUntilTerminal(ctx, job.JobID)
			if err != nil {
				return err
			}
			return writeCommandJSON(command, job)
		},
	}
	command.Flags().StringVar(&configFile, "config", "./config.json", "config file path")
	command.Flags().StringVar(&input, "input", "", "input .tgfb file")
	command.Flags().StringVar(&conflict, "conflict", "fail", "path conflict policy: fail or replace")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "validate without uploading or publishing")
	return command
}

func openBackupRuntime(
	ctx context.Context,
	configFile string,
) (*backupmgr.Manager, func(), error) {
	serviceConfig, err := config.Parse(configFile)
	if err != nil {
		return nil, func() {}, fmt.Errorf("parse config: %w", err)
	}
	if err := serviceConfig.Validate(); err != nil {
		return nil, func() {}, fmt.Errorf("validate config: %w", err)
	}
	if err := idgen.Init(1); err != nil {
		return nil, func() {}, fmt.Errorf("init id generator: %w", err)
	}
	if err := db.InitDBContext(ctx, serviceConfig.DBFile); err != nil {
		return nil, func() {}, fmt.Errorf("open backup database: %w", err)
	}
	closeRuntime := func() { _ = db.Close() }
	managerFiles, ioCache, err := buildFileManager(ctx, serviceConfig)
	if err != nil {
		closeRuntime()
		return nil, func() {}, err
	}
	closeRuntime = func() {
		closeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheShutdownTimeout)
		defer cancel()
		_ = ioCache.Close(closeContext)
		_ = db.Close()
	}
	manager, err := backupmgr.New(
		db.GetClient(),
		managerFiles,
		toBackupManagerOptions(serviceConfig, managerFiles.BackupMaxPartSize()),
	)
	if err != nil {
		closeRuntime()
		return nil, func() {}, fmt.Errorf("create backup manager: %w", err)
	}
	return manager, closeRuntime, nil
}

func cliIdempotencyKey(kind string) string {
	return kind + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func backupFileSHA256(ctx context.Context, reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextCommandReader{ctx: ctx, reader: reader}); err != nil {
		return "", fmt.Errorf("hash backup input: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextCommandReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextCommandReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func copyBackupArtifact(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create backup output directory: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open completed backup artifact: %w", err)
	}
	defer func() { _ = input.Close() }()
	partial := destination + ".partial"
	output, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create backup output: %w", err)
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("copy backup output: %w", err)
	}
	if err := os.Rename(partial, destination); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("publish backup output: %w", err)
	}
	return nil
}

func writeCommandJSON(command *cobra.Command, value any) error {
	encoder := json.NewEncoder(command.OutOrStdout())
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}
