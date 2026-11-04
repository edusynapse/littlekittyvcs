package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestGitClient(t *testing.T) {
	api := newAPI(t, Config{})
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	repo, err := api.st.CreateRepo(ctx, mustStoreID(t), "site")
	if err != nil {
		t.Fatal(err)
	}
	rw := api.insertToken("rw", true, false, nil)
	ro := api.insertToken("ro", false, false, nil)

	var seen reqCapture
	seen.next = api.h
	srv := httptest.NewServer(&seen)
	t.Cleanup(srv.Close)
	remote := srv.URL + "/site.git"
	env := gitEnv(t)

	t.Run("empty bearer", func(t *testing.T) {
		stdout, stderr, err := runGit(t, env, "-c", bearer(rw), "ls-remote", remote)
		if err != nil || stdout != "" {
			t.Fatalf("err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
	})

	t.Run("protocol version 1", func(t *testing.T) {
		stdout, stderr, err := runGit(t, env, "-c", "protocol.version=1", "-c", bearer(rw), "ls-remote", remote)
		if err != nil || stdout != "" {
			t.Fatalf("err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
	})

	t.Run("basic challenge", func(t *testing.T) {
		before := seen.infoRefsCount()
		u, err := url.Parse(remote)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword("x", rw)
		stdout, stderr, err := runGit(t, env, "ls-remote", u.String())
		if err != nil || stdout != "" {
			t.Fatalf("err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
		got := seen.infoRefsSince(before)
		if len(got) != 2 || got[0] || !got[1] {
			t.Fatalf("info/refs authorization present: %v", got)
		}
		t.Logf("info/refs requests=%d first_authorization_present=%t second_authorization_present=%t", len(got), got[0], got[1])
	})

	t.Run("no credentials", func(t *testing.T) {
		stdout, stderr, err := runGit(t, env, "ls-remote", remote)
		if err == nil || !strings.Contains(stderr, "could not read Username") {
			t.Fatalf("err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
		t.Logf("no-credentials stderr: %s", redact(stderr))
	})

	t.Run("empty clone", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "repo")
		stdout, stderr, err := runGit(t, env, "-c", bearer(rw), "clone", remote, dest)
		if err != nil {
			t.Fatalf("err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
		info, statErr := os.Stat(filepath.Join(dest, ".git"))
		if statErr != nil || !info.IsDir() {
			t.Fatalf(".git: %v", statErr)
		}
		if !strings.Contains(stderr, "You appear to have cloned an empty repository.") {
			t.Fatalf("stderr=%s", redact(stderr))
		}
		t.Logf("empty-clone stderr: %s", redact(stderr))
	})

	t.Run("refs", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, api.raw)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		refs := []struct {
			name string
			b    byte
		}{
			{"refs/heads/main", 0x11},
			{"refs/heads/dev", 0x22},
			{"refs/tags/v1", 0x33},
		}
		for _, ref := range refs {
			objectID := bytes.Repeat([]byte{ref.b}, 20)
			if _, err := conn.Exec(ctx, `
				insert into refs (repo_id, name, object_id) values ($1, $2, $3)`,
				repo.ID, ref.name, objectID); err != nil {
				t.Fatal(err)
			}
		}
		h11 := strings.Repeat("11", 20)
		h22 := strings.Repeat("22", 20)
		h33 := strings.Repeat("33", 20)
		want := h11 + "\tHEAD\n" +
			h22 + "\trefs/heads/dev\n" +
			h11 + "\trefs/heads/main\n" +
			h33 + "\trefs/tags/v1\n"
		stdout, stderr, err := runGit(t, env, "-c", bearer(rw), "ls-remote", remote)
		if err != nil || stdout != want {
			t.Fatalf("err=%v\nstdout:\n%s\nwant:\n%s\nstderr=%s", err, stdout, want, redact(stderr))
		}
		t.Logf("ls-remote:\n%s", stdout)
		stdout, stderr, err = runGit(t, env, "-c", bearer(rw), "ls-remote", "--symref", remote, "HEAD")
		if err != nil {
			t.Fatalf("err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
		first, _, _ := strings.Cut(stdout, "\n")
		if first != "ref: refs/heads/main\tHEAD" {
			t.Fatalf("symref first line %q\n%s", first, stdout)
		}
		t.Logf("ls-remote --symref:\n%s", stdout)
	})

	t.Run("read-only push", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "local")
		if stdout, stderr, err := runGit(t, env, "init", dir); err != nil {
			t.Fatalf("init err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
		if stdout, stderr, err := runGit(t, env,
			"-C", dir,
			"-c", "user.name=littlekittyvcs",
			"-c", "user.email=littlekittyvcs@example.com",
			"commit", "--allow-empty", "-m", "init"); err != nil {
			t.Fatalf("commit err=%v stdout=%q stderr=%s", err, stdout, redact(stderr))
		}
		posts := seen.postCount()
		stdout, stderr, err := runGit(t, env, "-C", dir, "-c", bearer(ro), "push", remote, "HEAD:refs/heads/main")
		if err == nil || !strings.Contains(stderr, "403") {
			t.Fatalf("err=%v stdout=%q stderr=%s", err, redact(stdout), redact(stderr))
		}
		if seen.postCount() != posts {
			t.Fatalf("push sent %d POST requests", seen.postCount()-posts)
		}
	})
}

func bearer(token string) string {
	return "http.extraHeader=Authorization: Bearer " + token
}

func gitEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"PATH=" + os.Getenv("PATH"),
	}
}

func runGit(t *testing.T, env []string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

var tokenRE = regexp.MustCompile(`lkv_[A-Za-z0-9_-]+`)

func redact(s string) string {
	return tokenRE.ReplaceAllString(s, "lkv_<redacted>")
}

// reqCapture counts info/refs calls and POST calls. It records only whether
// Authorization was present, never the header value.
type reqCapture struct {
	next     http.Handler
	mu       sync.Mutex
	infoRefs []bool
	posts    int
}

func (c *reqCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if r.Method == http.MethodPost {
		c.posts++
	}
	if strings.HasSuffix(r.URL.Path, "/info/refs") {
		c.infoRefs = append(c.infoRefs, r.Header.Get("Authorization") != "")
	}
	c.mu.Unlock()
	c.next.ServeHTTP(w, r)
}

func (c *reqCapture) infoRefsCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.infoRefs)
}

func (c *reqCapture) infoRefsSince(n int) []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]bool(nil), c.infoRefs[n:]...)
	return out
}

func (c *reqCapture) postCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posts
}
