package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/auth"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/gitx"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/link"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/skills"
)

func (a *App) login(args []string) int {
	fs := a.flags("login")
	urlFlag := fs.String("url", "", "ROS API base URL (default: the saved one, else "+config.DefaultURL+")")
	project := fs.String("project", "", "project to approve this device for (default: chosen on the approval page)")
	noBrowser := fs.Bool("no-browser", false, "don't open the sign-in page, just print it")
	deviceName := fs.String("device-name", "", "name shown in Settings → Devices (default: this machine's hostname)")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.errorf("reading config: %v", err)
	}
	if *urlFlag != "" {
		cfg.URL = config.NormalizeURL(*urlFlag)
		if !strings.HasPrefix(cfg.URL, "https://") && !isLoopback(cfg.URL) {
			return a.errorf("--url must be https:// (http:// is only allowed for localhost)")
		}
	}
	if a.Getenv("ROS_STORE") == "" {
		if (config.KeychainStore{}).Available() {
			cfg.Store = "keychain"
		} else {
			cfg.Store = "file"
			fmt.Fprintf(a.Stderr, "No OS keychain available; storing the refresh token in %s/credentials.json (mode 0600).\n",
				config.Home())
		}
	}
	name := *deviceName
	if name == "" {
		name, _ = os.Hostname()
	}
	sess := a.session(cfg)
	opts := auth.LoginOptions{ProjectID: *project, DeviceName: name, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Version: a.Version, Out: a.Stdout, Sleep: a.LoginSleep}
	if !*noBrowser {
		opts.OpenBrowser = a.OpenBrowser
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	tok, err := sess.Login(ctx, opts)
	if err != nil {
		return a.errorf("%v", err)
	}
	cfg.DeviceID, cfg.DefaultProjectID, cfg.SignedInAt = tok.DeviceID, tok.ProjectID, time.Now().UTC()
	cfg.Scopes = strings.Fields(tok.Scope)

	c := a.client(cfg.URL, tok.AccessToken)
	me, err := c.Me(ctx)
	if err == nil {
		cfg.UserEmail = me.User.Email
		if me.DefaultProjectID != "" {
			cfg.DefaultProjectID = me.DefaultProjectID
		}
	}
	if err := cfg.Save(); err != nil {
		return a.errorf("saving config: %v", err)
	}
	who := cfg.UserEmail
	if who == "" {
		who = "your account"
	}
	fmt.Fprintf(a.Stdout, "\nSigned in to %s as %s.\n", cfg.URL, who)
	if me != nil && len(me.Projects) == 0 {
		fmt.Fprintln(a.Stdout, "No project has the local agent enabled yet; ask a project admin to turn it on.")
	}

	// Set up the repo we're in, ahead of the first session (spec §7: the skills dir must exist at start).
	if repo, err := gitx.Find(a.cwd()); err == nil && me != nil {
		if l, err := linkRepo(ctx, me, repo, "", nil, a.Stdout); err == nil {
			fmt.Fprintf(a.Stdout, "Linked %s to project %q.\n", repo.Root, l.ProjectName)
		} else if _, perr := link.Prepare(repo); perr == nil {
			fmt.Fprintf(a.Stdout, "Not linked yet (%v); run `ros-local link` in this repo.\n", err)
		}
	}
	return 0
}

func isLoopback(u string) bool {
	for _, p := range []string{"http://localhost", "http://127.0.0.1", "http://[::1]"} {
		if u == p || strings.HasPrefix(u, p+":") || strings.HasPrefix(u, p+"/") {
			return true
		}
	}
	return false
}

func (a *App) logout(args []string) int {
	if a.flags("logout").Parse(args) != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.errorf("reading config: %v", err)
	}
	if err := config.StoreFor(cfg).Delete(cfg.URL); err != nil {
		return a.errorf("removing credentials: %v", err)
	}
	cfg.DeviceID, cfg.UserEmail, cfg.Scopes, cfg.SignedInAt = "", "", nil, time.Time{}
	if err := cfg.Save(); err != nil {
		return a.errorf("saving config: %v", err)
	}
	fmt.Fprintln(a.Stdout, "Signed out on this machine. To revoke the device everywhere, use Settings → Devices in ROS.")
	return 0
}

// linkRepo picks the project for repo (spec §3): an explicit id, else the project whose code repos match
// the git remote, else the default project, else the only project, else ask (when `ask` is non-nil).
func linkRepo(ctx context.Context, me *api.Me, repo *gitx.Repo, explicit string, ask io.Reader, out io.Writer) (*link.Link, error) {
	byID := map[string]api.Project{}
	for _, p := range me.Projects {
		byID[p.ID] = p
	}
	key := gitx.RepoKey(repo.Remote(ctx))
	l := &link.Link{Repo: key}
	switch {
	case explicit != "":
		p, ok := byID[explicit]
		if !ok {
			return nil, fmt.Errorf("project %s not found, or the local agent isn't enabled for it", explicit)
		}
		l.ProjectID, l.ProjectName, l.Source = p.ID, p.Name, "explicit"
	default:
		for _, r := range me.Repos {
			if key != "" && r.Repo == key {
				if p, ok := byID[r.ProjectID]; ok {
					l.ProjectID, l.ProjectName, l.Source = p.ID, p.Name, "remote"
				}
				break
			}
		}
		if l.ProjectID == "" && me.DefaultProjectID != "" {
			if p, ok := byID[me.DefaultProjectID]; ok {
				l.ProjectID, l.ProjectName, l.Source = p.ID, p.Name, "default"
			}
		}
		if l.ProjectID == "" && len(me.Projects) == 1 {
			l.ProjectID, l.ProjectName, l.Source = me.Projects[0].ID, me.Projects[0].Name, "default"
		}
		if l.ProjectID == "" && len(me.Projects) > 1 && ask != nil {
			p, err := choose(me.Projects, ask, out)
			if err != nil {
				return nil, err
			}
			l.ProjectID, l.ProjectName, l.Source = p.ID, p.Name, "prompt"
		}
	}
	if l.ProjectID == "" {
		if len(me.Projects) == 0 {
			return nil, errors.New("no project has the local agent enabled")
		}
		return nil, errors.New("several projects match; pass --project <id>")
	}
	if err := link.Write(repo, l); err != nil {
		return nil, err
	}
	if _, err := link.Prepare(repo); err != nil {
		return nil, err
	}
	return l, nil
}

func choose(projects []api.Project, in io.Reader, out io.Writer) (api.Project, error) {
	fmt.Fprintln(out, "Which ROS project is this repo for?")
	for i, p := range projects {
		fmt.Fprintf(out, "  %d) %s\n", i+1, p.Name)
	}
	fmt.Fprint(out, "> ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(projects) {
		return api.Project{}, errors.New("no project chosen; pass --project <id>")
	}
	return projects[n-1], nil
}

func (a *App) signedInClient(ctx context.Context) (*config.Config, *api.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	token, err := a.session(cfg).AccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	return cfg, a.client(cfg.URL, token), nil
}

func (a *App) link(args []string) int {
	fs := a.flags("link")
	project := fs.String("project", "", "project id (default: matched from the git remote, else your default project)")
	if fs.Parse(args) != nil {
		return 2
	}
	repo, err := gitx.Find(a.cwd())
	if err != nil {
		return a.errorf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, c, err := a.signedInClient(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	me, err := c.Me(ctx)
	if err != nil {
		return a.errorf("asking ROS which projects you can use: %v", err)
	}
	var ask io.Reader
	if isTerminal(a.Stdin) {
		ask = a.Stdin
	}
	l, err := linkRepo(ctx, me, repo, *project, ask, a.Stdout)
	if err != nil {
		return a.errorf("%v", err)
	}
	fmt.Fprintf(a.Stdout, "Linked %s to project %q (%s).\n", repo.Root, l.ProjectName, l.ProjectID)
	return 0
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (a *App) sync(args []string) int {
	if a.flags("sync").Parse(args) != nil {
		return 2
	}
	repo, err := gitx.Find(a.cwd())
	if err != nil {
		return a.errorf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, c, err := a.signedInClient(ctx)
	if err != nil {
		return a.errorf("%v", err)
	}
	l, err := link.Read(repo)
	if err != nil {
		return a.errorf("reading the repo link: %v", err)
	}
	if l == nil {
		return a.errorf("this repo isn't linked to a ROS project; run `ros-local link`")
	}
	created, err := link.Prepare(repo)
	if err != nil {
		return a.errorf("%v", err)
	}
	res, err := skills.Sync(ctx, c, repo.Root, skills.Options{ProjectID: l.ProjectID})
	if err != nil {
		return a.errorf("syncing skills: %v", err)
	}
	fmt.Fprintln(a.Stdout, describeSync(res))
	for _, w := range syncNotes(res, created) {
		fmt.Fprintln(a.Stdout, w)
	}
	return 0
}

func describeSync(r *skills.Result) string {
	if r.NotModified {
		return fmt.Sprintf("ROS skills up to date (%d).", r.Total)
	}
	return fmt.Sprintf("ROS skills: %d synced (%d added, %d updated, %d removed).",
		r.Total, len(r.Added), len(r.Updated), len(r.Removed))
}

// syncNotes are the lines worth showing the developer after a sync.
func syncNotes(r *skills.Result, createdDir bool) []string {
	var notes []string
	if createdDir && (len(r.Added) > 0 || len(r.Updated) > 0) {
		notes = append(notes, "ROS skills installed — run /reload-skills")
	}
	for _, name := range r.Edited {
		if dst, ok := r.BackedUp[name]; ok {
			notes = append(notes, fmt.Sprintf("ROS skill %s was edited locally; your copy is in %s", name, dst))
		} else {
			notes = append(notes, fmt.Sprintf("ROS skill %s is edited locally; it will be replaced (with a backup) when ROS updates it", name))
		}
	}
	return append(notes, r.Warnings...)
}

func (a *App) status(args []string) int {
	if a.flags("status").Parse(args) != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		return a.errorf("reading config: %v", err)
	}
	store := config.StoreFor(cfg)
	w := a.Stdout
	fmt.Fprintf(w, "ros-local %s\n", a.Version)
	fmt.Fprintf(w, "ROS:        %s\n", cfg.URL)
	if mcp := a.Getenv("ROS_URL"); mcp != "" && config.NormalizeURL(mcp) != cfg.URL {
		fmt.Fprintf(w, "            warning: ROS_URL is %s, so the plugin's MCP server points elsewhere\n", mcp)
	}
	_, err = store.Get(cfg.URL)
	switch {
	case errors.Is(err, config.ErrNoCredentials):
		fmt.Fprintln(w, "Signed in:  no — run `ros-local login`")
	case err != nil:
		fmt.Fprintf(w, "Signed in:  unknown (%s: %v)\n", store.Name(), err)
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		token, terr := a.session(cfg).AccessToken(ctx)
		switch {
		case errors.Is(terr, auth.ErrSignedOut):
			fmt.Fprintln(w, "Signed in:  no (the device was signed out) — run `ros-local login`")
		case terr != nil:
			fmt.Fprintf(w, "Signed in:  yes, offline (%v)\n", terr)
		default:
			who := cfg.UserEmail
			if me, err := a.client(cfg.URL, token).Me(ctx); err == nil {
				who = me.User.Email
			}
			fmt.Fprintf(w, "Signed in:  %s (device %s, token in the %s)\n", who, cfg.DeviceID, store.Name())
			if c, _ := store.Get(cfg.URL); c != nil {
				fmt.Fprintf(w, "Token:      valid until %s\n", c.AccessExpiresAt.Local().Format(time.Kitchen))
			}
		}
	}
	repo, err := gitx.Find(a.cwd())
	if err != nil {
		fmt.Fprintln(w, "Repo:       not in a git repository")
		return 0
	}
	l, _ := link.Read(repo)
	if l == nil {
		fmt.Fprintf(w, "Repo:       %s (not linked; the next session links it, or run `ros-local link`)\n", repo.Root)
	} else {
		fmt.Fprintf(w, "Repo:       %s → project %q (%s)\n", repo.Root, l.ProjectName, l.ProjectID)
	}
	if m, err := skills.ReadManifest(skills.Dir(repo.Root)); err == nil && len(m.Skills) > 0 {
		fmt.Fprintf(w, "Skills:     %d synced %s\n", len(m.Skills), ago(m.SyncedAt, time.Now()))
	}
	return 0
}
