package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	flywheel "github.com/cobr-io/flywheel"
	"github.com/cobr-io/flywheel/internal/cli/add/app"
	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/clean"
	"github.com/cobr-io/flywheel/internal/cli/converge"
	"github.com/cobr-io/flywheel/internal/cli/doctor"
	"github.com/cobr-io/flywheel/internal/cli/down"
	"github.com/cobr-io/flywheel/internal/cli/hostmount"
	cliInit "github.com/cobr-io/flywheel/internal/cli/initcmd"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/publishapp"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/cobr-io/flywheel/internal/cli/up"
	"github.com/cobr-io/flywheel/internal/cli/usecmd"
	"github.com/cobr-io/flywheel/internal/cli/worktree"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// BuildVersion is stamped by `make install` (git describe); an
			// unstamped `go build` reports the default "v0.0.0-dev".
			fmt.Println("flywheel " + flywheel.BuildVersion)
			return nil
		},
	}
}

func newDoctorCmd() *cobra.Command {
	var quick bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites",
		RunE: func(cmd *cobra.Command, args []string) error {
			var checks []doctor.Check
			if quick {
				checks = doctor.QuickChecks()
			} else {
				// Full mode includes allocator port-collision check.
				checks = doctor.FullChecks("")
			}
			results := doctor.Run(checks)
			bad := 0
			for _, r := range results {
				label := fmt.Sprintf("%-8s — %s", r.Check.Name, r.Check.Description)
				if r.OK() {
					style.OK(os.Stdout, "%s", label)
					continue
				}
				bad++
				style.Err(os.Stdout, "%s", label)
				style.Detail(os.Stdout, "  %v", r.Err)
			}
			if bad > 0 {
				return fmt.Errorf("%d prerequisite check(s) failed", bad)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&quick, "quick", false, "run only the minimal subset (matches `up` step 2)")
	return cmd
}

func newInitCmd() *cobra.Command {
	var org, version string
	cmd := &cobra.Command{
		Use:   "init [<path>]",
		Short: "Scaffold a client repo. No arg: initialise cwd. Arg: create/use <path>.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Cwd-or-path semantics:
			//   `flywheel init`         → initialise cwd; name = basename(cwd).
			//   `flywheel init <path>`  → mkdir+initialise <path>; name = basename.
			target := ""
			if len(args) >= 1 {
				target = args[0]
			}
			// Refuse to scaffold into a host path Docker Desktop can't bind-mount
			// into k3d (macOS temp dirs) — the resulting repo could never `up`.
			checkDir := target
			if checkDir == "" {
				if wd, err := os.Getwd(); err == nil {
					checkDir = wd
				}
			}
			if err := hostmount.Guard("scaffold a flywheel repo", checkDir); err != nil {
				return err
			}
			res, err := cliInit.Run(cliInit.Options{
				TargetDir: target, // empty → cwd, set → that path
				Org:       org,
				Version:   version,
				// SkeletonFS unset → embedded client-skeleton.
				FlywheelRepoURL: os.Getenv("FLYWHEEL_REPO_URL"), // empty = default github.com/cobr-io/flywheel
			})
			if err != nil {
				return err
			}
			style.Summary(os.Stdout, "initialised %s", res.RepoDir)
			style.Detail(os.Stdout, "flywheel: %s @ %s", res.FlywheelTag, res.FlywheelSHA[:12])
			style.Detail(os.Stdout, "ports:    registry=%d http=%d https=%d",
				res.Triple.RegistryPort, res.Triple.HttpPort, res.Triple.HttpsPort)
			style.Detail(os.Stdout, "age key:  %s", res.AgeKeyPath)
			if res.HooksNote != "" {
				style.Detail(os.Stdout, "hooks:    %s", res.HooksNote)
			}
			fmt.Println()
			style.Summary(os.Stdout, "next: %s", res.NextSteps)
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "GitHub org hint (optional)")
	cmd.Flags().StringVar(&version, "version", "", "Flywheel version to pin to (default: latest release tag)")
	return cmd
}

func newUpCmd() *cobra.Command {
	var wait, clone, noClone bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Reconcile cluster to flywheel.yaml",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			// Refuse early from a host path Docker Desktop can't bind-mount into
			// k3d (macOS temp dirs); up also re-verifies the mount post-create.
			if err := hostmount.Guard("run flywheel up", wd); err != nil {
				return err
			}
			// nil = ask on a TTY / skip otherwise; explicit --clone/--no-clone set it.
			var clonePtr *bool
			if cmd.Flags().Changed("clone") || cmd.Flags().Changed("no-clone") {
				v := clone && !noClone
				clonePtr = &v
			}
			// nil = default (wait); explicit --wait/--wait=false set it.
			var waitPtr *bool
			if cmd.Flags().Changed("wait") {
				waitPtr = &wait
			}
			return up.Run(context.Background(), up.Options{
				RepoDir: wd,
				Wait:    waitPtr,
				Clone:   clonePtr,
				Stdout:  os.Stdout,
				Stdin:   os.Stdin,
			})
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for Flux Kustomizations Ready before returning")
	cmd.Flags().BoolVar(&clone, "clone", false, "clone missing app worktrees from their recorded source")
	cmd.Flags().BoolVar(&noClone, "no-clone", false, "skip cloning missing app worktrees")
	return cmd
}

func newDownCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Delete cluster + registry (destructive)",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			return down.Down(context.Background(), down.Options{
				RepoDir: wd,
				Yes:     yes,
			}, os.Stdout)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip confirmation prompt")
	return cmd
}

func newCleanCmd() *cobra.Command {
	var orphaned bool
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Opt-in destructive cleanup of orphaned PVCs",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			cfg, err := readClusterConfig(wd)
			if err != nil {
				return err
			}
			a, err := applier.New("", k3d.KubeContext(cfg.Cluster.Name))
			if err != nil {
				return err
			}
			return clean.Run(context.Background(), a, clean.Options{
				FlywheelNamespace: cfg.Namespaces.Flywheel,
				Orphaned:          orphaned,
			}, os.Stdout)
		},
	}
	cmd.Flags().BoolVar(&orphaned, "orphaned", true, "delete orphaned PVCs")
	return cmd
}

// newAddCmd is the parent for the `add` command family. Today its only child is
// `add app`; `add env` and friends may follow. Running bare `add` prints help
// and exits 2, mirroring the root's no-subcommand behavior.
func newAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <resource>",
		Short: "Add a resource (app) to the workspace",
		Long:  "Add a resource to the workspace. The only resource today is `app`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = cmd.Help()
			return exitError{code: 2, err: errSilent}
		},
	}
	cmd.AddCommand(newAddAppCmd())
	return cmd
}

func newAddAppCmd() *cobra.Command {
	var name, image, buildContext, dockerfile, target, namespace, branch string
	cmd := &cobra.Command{
		Use:   "app <dir>",
		Short: "Scaffold a per-app builder from a worktree directory",
		Long: "Scaffold the builder + workload for a host worktree directory under " +
			"workspaces_root.\n\n<dir> may be a bare name (a child of workspaces_root), a " +
			"relative path, or an absolute path. The app name is derived from a project " +
			"manifest in <dir> (package.json, pyproject.toml, setup.cfg, go.mod, Cargo.toml, " +
			"composer.json, pom.xml, *.gemspec), falling back to the directory name; " +
			"override with --name.",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeWorktreeDirs,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			res, err := app.Run(app.Options{
				RepoDir:    wd,
				Worktree:   args[0],
				Name:       name,
				Image:      image,
				Context:    buildContext,
				Dockerfile: dockerfile,
				Target:     target,
				Namespace:  namespace,
				Branch:     branch,
				Stdout:     os.Stdout,
			})
			if err != nil {
				return err
			}
			fmt.Println()
			style.Summary(os.Stdout, "next: %s", res.NextSteps)
			style.Summary(os.Stdout, "url:  %s   (once the pod is running)", res.URL)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "app name (default: derived from a manifest in <dir>, else the dir name)")
	cmd.Flags().StringVar(&image, "image", "", "image short name (defaults to the app name)")
	cmd.Flags().StringVar(&buildContext, "context", ".", "docker build context, relative to the worktree")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "Dockerfile", "Dockerfile path within --context")
	cmd.Flags().StringVar(&target, "target", "", "multi-stage build target stage (default: the Dockerfile's last stage)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Kubernetes namespace for the workload (default: namespaces.apps from flywheel.yaml)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to check out on clone and record for this app (default: the remote's default branch)")
	return cmd
}

func newPublishAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish-app <name>",
		Short: "Promote a local-only app to remote-backed",
		Long: "Once a local-only app's worktree has been pushed to an external remote, " +
			"publish-app flips its source from local-only to that remote URL — clearing " +
			"the local-only guard so the app can be merged to the integration branch.\n\n" +
			"Requires the worktree to have a reachable 'origin' AND its current branch " +
			"to be pushed.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeLocalOnlyApps,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			return publishapp.Run(publishapp.Options{RepoDir: wd, Name: args[0], Stdout: os.Stdout})
		},
	}
	return cmd
}

// completeLocalOnlyApps completes publish-app's <name> arg with the registered
// apps that are currently local-only (the only ones worth publishing).
// Best-effort: any failure yields no candidates.
func completeLocalOnlyApps(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	cfg, err := converge.LoadConfig(wd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	localOnly := make(map[string]bool)
	for _, name := range cfg.LocalOnlyWorktrees() {
		localOnly[name] = true
	}
	apps, err := worktree.DeclaredApps(wd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, a := range apps {
		if localOnly[a.Worktree] {
			names = append(names, a.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func newUseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "use <branch>",
		Short: "Choose which gitops branch Flux deploys",
		Long: "Repoint the cluster's self GitRepository at <branch> so Flux deploys it.\n\n" +
			"The gitops/self git-auto-sync does NOT auto-follow local checkouts: a transient " +
			"checkout (e.g. the one `git rebase` does) would otherwise deploy — and, with " +
			"prune:true, tear down — an infra-less branch tip. Use this to deploy a branch " +
			"deliberately; switch back with `flywheel use main`. If the deployed branch is " +
			"later deleted, git-auto-sync degrades to the default branch automatically.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeBranches,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			return usecmd.Run(context.Background(), usecmd.Options{
				RepoDir: wd,
				Branch:  args[0],
				Stdout:  os.Stdout,
			})
		},
	}
	return cmd
}

// completeBranches powers `flywheel use <TAB>` by listing the gitops repo's
// local branches. Best-effort: any failure yields no candidates so the shell
// falls back gracefully.
func completeBranches(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	branches, err := usecmd.LocalBranches(wd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return branches, cobra.ShellCompDirectiveNoFileComp
}

// completeWorktreeDirs powers shell completion of the `add app` <dir> argument by
// listing git-worktree directories under workspaces_root. Best-effort: any
// failure yields no candidates so the shell falls back gracefully.
func completeWorktreeDirs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	dirs, err := app.WorkspaceDirs(wd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return dirs, cobra.ShellCompDirectiveNoFileComp
}

// readClusterConfig reads + parses flywheel.yaml (no full validation) for
// commands that just need cluster.name + namespaces.
func readClusterConfig(repoDir string) (*schema.File, error) {
	raw, err := os.ReadFile(filepath.Join(repoDir, "flywheel.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read flywheel.yaml: %w", err)
	}
	f, err := schema.Parse(raw)
	if err != nil {
		return nil, err
	}
	if f.Cluster.Name == "" {
		return nil, fmt.Errorf("flywheel.yaml missing cluster.name")
	}
	if f.Namespaces.Flywheel == "" {
		f.Namespaces.Flywheel = "flywheel-system"
	}
	return f, nil
}
