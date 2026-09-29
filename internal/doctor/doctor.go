// Package doctor implements `slack-tui doctor`: a diagnostic report that
// catches the real-world setup failures — stale env tokens overriding the
// fresh tokens.json, tokens issued before scope additions (missing_scope),
// config split across the new and legacy dirs, and a silently absent Socket
// Mode falling back to polling. It only reads; it never changes config.
package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kurenn/slack-tui/internal/auth"
	"github.com/kurenn/slack-tui/internal/config"
)

// featureFor maps a required user scope to the feature it unlocks, so a missing
// scope reads as the broken keystroke rather than an opaque OAuth string.
var featureFor = map[string]string{
	"reactions:write":     "a (react)",
	"search:read":         "s (search)",
	"channels:write":      "joining channels + marking public channels read",
	"groups:write":        "marking private channels read (Slack keeps them unread)",
	"im:write":            "opening DMs + marking DMs read (Slack keeps them unread)",
	"mpim:write":          "marking group DMs read (Slack keeps them unread)",
	"users:write":         "presence",
	"dnd:write":           "Do Not Disturb",
	"users.profile:write": "status text",
	"chat:write":          "sending messages",
	"files:read":          "downloading files",
	"files:write":         "attaching files",
	"canvases:read":       "reading Canvas sections",
	"canvases:write":      "creating and editing Canvases",
}

// httpClient is the client used for the network checks; a var so tests could
// stub it, but the pure logic is tested without touching it.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// Run prints the diagnostic report and returns a process exit code: 1 when
// there's no user token or auth.test fails, 0 otherwise (missing scopes are
// warnings, not failures). version is passed in by main (versionString()).
func Run(version string) int {
	fmt.Printf("slack-tui doctor  %s\n", version)

	reportConfig()

	file, _ := config.LoadTokens()
	env := envTokens()
	reportTokens(file, env)

	resolved := file.Resolve()
	exit := 0

	if resolved.User == "" {
		fmt.Println("\nAuth")
		fmt.Println("  ✗ no user token — sign in with `slack-tui login`")
		return 1
	}

	info, scopes, err := authTest(resolved.User)
	fmt.Println("\nAuth")
	if err != nil {
		fmt.Printf("  ✗ auth.test failed: %s\n", err)
		exit = 1
	} else {
		fmt.Printf("  ✓ %s — %s (%s)\n", info.Team, info.User, info.UserID)
	}

	fmt.Println("\nScopes")
	if err != nil {
		fmt.Println("  – skipped (auth failed)")
	} else {
		missing := missingScopes(scopes, auth.UserScopes)
		if len(missing) == 0 {
			fmt.Println("  ✓ all required scopes granted")
		} else {
			for _, m := range missing {
				if feat := featureFor[m]; feat != "" {
					fmt.Printf("  ✗ missing %s — breaks %s\n", m, feat)
				} else {
					fmt.Printf("  ✗ missing %s\n", m)
				}
			}
		}
	}

	reportSocketMode(resolved)

	return exit
}

// reportConfig prints the config dir, which files exist, and any legacy dir.
func reportConfig() {
	fmt.Println("\nConfig")
	dir, err := config.Dir()
	if err != nil {
		fmt.Printf("  ✗ cannot resolve config dir: %s\n", err)
		return
	}
	fmt.Printf("  %s\n", dir)
	for _, name := range []string{"prefs.json", "tokens.json", "oauth.json"} {
		if fileExists(filepath.Join(dir, name)) {
			fmt.Printf("  ✓ %s\n", name)
		} else {
			fmt.Printf("  – %s (absent)\n", name)
		}
	}
	// On Linux with XDG_CONFIG_HOME set, os.UserConfigDir() returns the very
	// same path as config.Dir(), so the "legacy" dir is the current one and the
	// warning is noise — in the first command people run when something's wrong.
	if legacy := legacyConfigDir(); legacy != "" && legacy != dir && dirHasFiles(legacy) {
		fmt.Printf("  ! legacy config also present at %s\n", legacy)
		fmt.Println("    (reads fall back to it; writes go to the new dir)")
	}
}

// reportTokens classifies each token's source and prints a masked preview.
func reportTokens(file config.Tokens, env envValues) {
	fmt.Println("\nTokens")
	if all, active, _ := config.LoadWorkspaces(); len(all) > 0 {
		line := "  workspaces: "
		for i, w := range all {
			if i > 0 {
				line += ", "
			}
			line += w.Name
			if w.Name == active {
				line += " (active)"
			}
		}
		fmt.Println(line)
	}
	rows := []struct {
		label, fileVal, envVal string
	}{
		{"user", file.User, env.user},
		{"app", file.App, env.app},
		{"bot", file.Bot, env.bot},
	}
	for _, r := range rows {
		src := tokenSource(r.fileVal, r.envVal)
		eff := r.fileVal
		if r.envVal != "" {
			eff = r.envVal
		}
		marker := "✓ "
		switch src {
		case sourceOverriding:
			marker = "! "
		case sourceUnset:
			marker = "– "
		}
		if eff == "" {
			fmt.Printf("  %s%-4s %s\n", marker, r.label, src)
		} else {
			fmt.Printf("  %s%-4s %s  %s\n", marker, r.label, src, mask(eff))
		}
	}
	reportRotation(file)
}

// reportRotation explains the expiry on a PKCE sign-in. Slack forces rotation
// for the loopback redirect, so an expired token with no refresh token is the
// one state that needs a re-login rather than a retry.
func reportRotation(file config.Tokens) {
	switch {
	case !file.Rotating():
		fmt.Println("  – user token does not expire (pasted by hand)")
	case file.Refresh == "":
		fmt.Println("  ✗ user token expires but has no refresh token — run `slack-tui login`")
	default:
		left := time.Until(time.Unix(file.ExpiresAt, 0))
		if left <= 0 {
			fmt.Println("  ! user token expired — it refreshes automatically on next launch")
			return
		}
		fmt.Printf("  ✓ user token expires in %s (auto-refreshed within %s of expiry)\n",
			left.Round(time.Minute), auth.RefreshSkew)
	}
}

// reportSocketMode probes apps.connections.open when both app+bot resolve.
func reportSocketMode(resolved config.Tokens) {
	fmt.Println("\nSocket Mode")
	if resolved.App == "" || resolved.Bot == "" {
		fmt.Println("  – not configured: live channel unread falls back to polling")
		return
	}
	if err := connectionsOpen(resolved.App); err != nil {
		fmt.Printf("  ✗ %s\n", err)
		return
	}
	fmt.Println("  ✓ Socket Mode available (live unread)")
}

// --- pure helpers (unit-tested without network) ---

type tokenSourceKind string

const (
	sourceEnv        tokenSourceKind = "env"
	sourceFile       tokenSourceKind = "file"
	sourceOverriding tokenSourceKind = "env (overriding file!)"
	sourceUnset      tokenSourceKind = "not set"
)

// tokenSource classifies where the effective token comes from. Env wins per
// token (matches config.Tokens.Resolve); when env shadows a stored file token
// that's the dangerous "stale env" case users keep hitting.
func tokenSource(fileVal, envVal string) tokenSourceKind {
	switch {
	case envVal != "" && fileVal != "":
		return sourceOverriding
	case envVal != "":
		return sourceEnv
	case fileVal != "":
		return sourceFile
	default:
		return sourceUnset
	}
}

func (k tokenSourceKind) String() string { return string(k) }

// missingScopes returns required scopes not present in granted, in the order
// they appear in required.
func missingScopes(granted, required []string) []string {
	have := make(map[string]bool, len(granted))
	for _, g := range granted {
		if g = strings.TrimSpace(g); g != "" {
			have[g] = true
		}
	}
	var missing []string
	for _, r := range required {
		if !have[r] {
			missing = append(missing, r)
		}
	}
	return missing
}

// mask renders a token as its type prefix plus the last 4 chars, never the
// secret middle. Short or empty tokens degrade gracefully.
func mask(tok string) string {
	if tok == "" {
		return ""
	}
	prefix := tok
	if i := strings.Index(tok, "-"); i > 0 {
		prefix = tok[:i]
	}
	last := tok
	if len(tok) > 4 {
		last = tok[len(tok)-4:]
	}
	return fmt.Sprintf("%s-…%s", prefix, last)
}

type envValues struct{ user, app, bot string }

func envTokens() envValues {
	return envValues{
		user: os.Getenv("SLACK_USER_TOKEN"),
		app:  os.Getenv("SLACK_APP_TOKEN"),
		bot:  os.Getenv("SLACK_BOT_TOKEN"),
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// legacyConfigDir mirrors config.legacyDir (unexported) — the pre-v0.1.7
// os.UserConfigDir location. Returns "" if it can't be resolved.
func legacyConfigDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "slack-tui")
}

func dirHasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

// --- network helpers ---

type authInfo struct {
	Team   string
	User   string
	UserID string
}

// authTest calls auth.test and returns identity + the granted scopes from the
// X-OAuth-Scopes response header.
func authTest(userToken string) (authInfo, []string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://slack.com/api/auth.test", nil)
	if err != nil {
		return authInfo{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return authInfo{}, nil, err
	}
	defer resp.Body.Close()

	var body struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Team   string `json:"team"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return authInfo{}, nil, err
	}
	if !body.OK {
		return authInfo{}, nil, fmt.Errorf("%s", body.Error)
	}
	scopes := splitScopes(resp.Header.Get("X-OAuth-Scopes"))
	return authInfo{Team: body.Team, User: body.User, UserID: body.UserID}, scopes, nil
}

// connectionsOpen probes Socket Mode availability with the app-level token.
func connectionsOpen(appToken string) error {
	req, err := http.NewRequest(http.MethodPost, "https://slack.com/api/apps.connections.open", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+appToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if !body.OK {
		return fmt.Errorf("%s", body.Error)
	}
	return nil
}

// splitScopes parses the X-OAuth-Scopes header — Slack sends it
// comma-separated without spaces ("identify,chat:write,…").
func splitScopes(header string) []string {
	if header == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(header, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
