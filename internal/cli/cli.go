package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
	ucli "github.com/urfave/cli"
)

func Run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	root := defaultRoot()
	if v := os.Getenv("ARTIFACT_FS_ROOT"); v != "" {
		root = v
	}

	app := ucli.NewApp()
	app.Name = "artifact-fs"
	app.Usage = "Git-backed FUSE filesystem with persistent writable overlay"
	app.Writer = stdout
	app.ErrWriter = stderr
	app.Metadata = map[string]interface{}{"ctx": ctx, "root": root}

	app.Commands = []ucli.Command{
		{
			Name:  "daemon",
			Usage: "start the artifact-fs daemon",
			Flags: []ucli.Flag{
				ucli.StringFlag{Name: "root", Value: filepath.Join(root, "mnt"), Usage: "mount root directory"},
				ucli.IntFlag{Name: "hydration-concurrency", Value: daemon.DefaultHydrationConcurrency, Usage: "number of concurrent blob hydration workers"},
			},
			Action: func(c *ucli.Context) error {
				logger := logging.NewJSONLogger(stderr, slog.LevelInfo)
				svc, err := daemon.New(ctx, root, logger)
				if err != nil {
					return err
				}
				defer svc.Close()
				svc.SetMountRoot(c.String("root"))
				svc.SetHydrationConcurrency(c.Int("hydration-concurrency"))
				err = svc.Start(ctx)
				if err == context.Canceled {
					return nil
				}
				return err
			},
		},
		{
			Name:  "add-repo",
			Usage: "add and mount a repository",
			Flags: []ucli.Flag{
				ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				ucli.StringFlag{Name: "remote", Usage: "remote URL (required)"},
				ucli.StringFlag{Name: "branch", Value: "main", Usage: "branch to track"},
				ucli.StringFlag{Name: "refresh", Value: "30s", Usage: "refresh interval"},
				ucli.StringFlag{Name: "mount-root", Usage: "override mount root"},
			},
			Action: withService(ctx, root, stderr, func(c *ucli.Context, svc *daemon.Service) error {
				name := strings.TrimSpace(c.String("name"))
				remote := strings.TrimSpace(c.String("remote"))
				if name == "" || remote == "" {
					return fmt.Errorf("--name and --remote are required")
				}
				d, err := daemon.ParseRefresh(c.String("refresh"))
				if err != nil {
					return err
				}
				cfg := model.RepoConfig{
					Name:            name,
					ID:              model.RepoID(name),
					RemoteURL:       remote,
					Branch:          c.String("branch"),
					RefreshInterval: d,
					MountRoot:       c.String("mount-root"),
					Enabled:         true,
				}
				if err := svc.AddRepo(ctx, cfg); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "added %s\n", cfg.Name)
				return nil
			}),
		},
		nameCommand("remove-repo", "remove a repository", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.RemoveRepo(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "removed %s\n", name)
			return nil
		}),
		nameCommand("status", "show repo status", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			st, err := svc.Status(c, name)
			if err != nil {
				return err
			}
			fmt.Fprintln(w, formatStatusLine(st))
			return nil
		}),
		nameCommand("fetch", "fetch remote updates", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.FetchNow(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "fetched %s\n", name)
			return nil
		}),
		{
			Name:  "list-repos",
			Usage: "list configured repos",
			Action: withService(ctx, root, stderr, func(c *ucli.Context, svc *daemon.Service) error {
				repos, err := svc.ListRepos(ctx)
				if err != nil {
					return err
				}
				for _, r := range repos {
					fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", r.Name, r.Branch, r.MountPath, r.RemoteURLRedacted)
				}
				return nil
			}),
		},
		nameCommand("remount", "remount a repo", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.Remount(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "remounted %s\n", name)
			return nil
		}),
		nameCommand("unmount", "unmount a repo", ctx, root, stderr, stdout, func(c context.Context, svc *daemon.Service, name string, w io.Writer) error {
			if err := svc.Unmount(c, name); err != nil {
				return err
			}
			fmt.Fprintf(w, "unmounted %s\n", name)
			return nil
		}),
		{
			Name:  "set-refresh",
			Usage: "update refresh interval",
			Flags: []ucli.Flag{
				ucli.StringFlag{Name: "name", Usage: "repo name (required)"},
				ucli.StringFlag{Name: "interval", Usage: "refresh interval (required)"},
			},
			Action: withService(ctx, root, stderr, func(c *ucli.Context, svc *daemon.Service) error {
				name := strings.TrimSpace(c.String("name"))
				interval := strings.TrimSpace(c.String("interval"))
				if name == "" || interval == "" {
					return fmt.Errorf("--name and --interval required")
				}
				d, err := daemon.ParseRefresh(interval)
				if err != nil {
					return err
				}
				if err := svc.SetRefresh(ctx, name, d); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "updated refresh %s %s\n", name, d)
				return nil
			}),
		},
		stubCommand("evict-cache", "evict blob cache", stdout),
		stubCommand("gc", "garbage collect", stdout),
		stubCommand("doctor", "check repo health", stdout),
		stubCommand("prefetch", "prefetch blobs", stdout),
	}

	if err := app.Run(append([]string{"artifact-fs"}, args...)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func defaultRoot() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", "artifact-fs")
		}
	}
	return "/var/lib/artifact-fs"
}

// withService creates a daemon.Service for the duration of the action.
func withService(ctx context.Context, root string, stderr io.Writer, fn func(*ucli.Context, *daemon.Service) error) ucli.ActionFunc {
	return func(c *ucli.Context) error {
		logger := logging.NewJSONLogger(stderr, slog.LevelInfo)
		svc, err := daemon.New(ctx, root, logger)
		if err != nil {
			return err
		}
		defer svc.Close()
		return fn(c, svc)
	}
}

// nameCommand builds a subcommand that takes a --name flag and runs fn.
func nameCommand(name, usage string, ctx context.Context, root string, stderr io.Writer, stdout io.Writer, fn func(context.Context, *daemon.Service, string, io.Writer) error) ucli.Command {
	return ucli.Command{
		Name:  name,
		Usage: usage,
		Flags: []ucli.Flag{ucli.StringFlag{Name: "name", Usage: "repo name (required)"}},
		Action: withService(ctx, root, stderr, func(c *ucli.Context, svc *daemon.Service) error {
			n := strings.TrimSpace(c.String("name"))
			if n == "" {
				return fmt.Errorf("--name required")
			}
			return fn(ctx, svc, n, stdout)
		}),
	}
}

func formatStatusLine(st model.RepoRuntimeState) string {
	return fmt.Sprintf("repo=%s state=%s head=%s ref=%s ahead=%d behind=%d diverged=%t last_fetch=%s result=%s hydrated_blobs=%d hydrated_bytes=%d overlay_dirty=%t",
		st.RepoID, st.State, st.CurrentHEADOID, st.CurrentHEADRef,
		st.AheadCount, st.BehindCount, st.Diverged,
		formatLastFetchAt(st.LastFetchAt), formatLastFetchResult(st.LastFetchResult),
		st.HydratedBlobCount, st.HydratedBlobBytes, st.DirtyOverlay)
}

func formatLastFetchAt(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.Format(time.RFC3339)
}

func formatLastFetchResult(result string) string {
	if strings.TrimSpace(result) == "" {
		return "never"
	}
	return result
}

func stubCommand(name, usage string, stdout io.Writer) ucli.Command {
	return ucli.Command{
		Name:  name,
		Usage: usage,
		Flags: []ucli.Flag{ucli.StringFlag{Name: "name", Usage: "repo name"}},
		Action: func(c *ucli.Context) error {
			fmt.Fprintf(stdout, "%s not implemented yet\n", name)
			return nil
		},
	}
}
