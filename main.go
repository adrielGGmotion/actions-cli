package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

const version = "0.1.0"

type Config struct {
	Repository string   `yaml:"repository" json:"repository"`
	Recipient  string   `yaml:"recipient" json:"recipient"`
	Outputs    []string `yaml:"outputs" json:"outputs"`
	Cache      []string `yaml:"cache" json:"cache"`
	Exclude    []string `yaml:"exclude" json:"exclude"`
}

type Request struct {
	Argv    []string `json:"argv"`
	WorkDir string   `json:"work_dir"`
	Outputs []string `json:"outputs"`
	Cache   []string `json:"cache"`
	ReplyTo string   `json:"reply_to"`
}

type Result struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

type release struct {
	ID        int64  `json:"id"`
	UploadURL string `json:"upload_url"`
	Assets    []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

type githubClient struct {
	token string
	repo  string
	http  *http.Client
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: remote <command...>")
		return 2
	}
	var err error
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("remote " + version)
		return 0
	case "init":
		err = initConfig()
	case "worker-keygen":
		err = keygen()
	case "worker":
		err = worker(os.Args[2:])
	default:
		return runRemote(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote:", err)
		return 1
	}
	return 0
}

func initConfig() error {
	if _, err := os.Stat(".remote.yml"); err == nil {
		return errors.New(".remote.yml already exists")
	}
	data := []byte("repository: owner/remote-worker\nrecipient: age1...\n\noutputs:\n  - build/outputs/**\n\ncache: []\nexclude: []\n")
	return os.WriteFile(".remote.yml", data, 0600)
}

func keygen() error {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	fmt.Println("GitHub secret REMOTE_AGE_IDENTITY:")
	fmt.Println(id.String())
	fmt.Println("Project config recipient:")
	fmt.Println(id.Recipient().String())
	return nil
}

func runRemote(argv []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	root, err := gitRoot(cwd)
	if err != nil {
		return fail(err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		return fail(err)
	}
	token, err := authToken(ctx)
	if err != nil {
		return fail(err)
	}
	recipient, err := age.ParseX25519Recipient(cfg.Recipient)
	if err != nil {
		return fail(fmt.Errorf("invalid recipient: %w", err))
	}
	replyID, err := age.GenerateX25519Identity()
	if err != nil {
		return fail(err)
	}
	jobID, err := randomID()
	if err != nil {
		return fail(err)
	}
	tmp, err := os.MkdirTemp("", "remote-"+jobID+"-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	requestPath := filepath.Join(tmp, "request.age")
	req := Request{Argv: argv, WorkDir: mustRel(root, cwd), Outputs: cfg.Outputs, Cache: cfg.Cache, ReplyTo: replyID.Recipient().String()}
	if err := makeRequest(ctx, root, requestPath, req, cfg.Exclude, recipient); err != nil {
		return fail(err)
	}
	gh := &githubClient{token: token, repo: cfg.Repository, http: &http.Client{Timeout: 90 * time.Second}}
	rel, err := gh.createRelease(ctx, jobID)
	if err != nil {
		return fail(err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = gh.deleteReleaseAndTag(context.Background(), rel.ID, jobID)
		}
	}()
	if err := gh.upload(ctx, rel.UploadURL, "request.age", requestPath); err != nil {
		return fail(err)
	}
	if err := gh.dispatch(ctx, jobID); err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stderr, "remote: job submitted")
	resultPath := filepath.Join(tmp, "result.age")
	if err := gh.waitResult(ctx, jobID, resultPath); err != nil {
		if errors.Is(err, context.Canceled) {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if cancelErr := gh.cancelJob(cancelCtx, jobID); cancelErr != nil {
				fmt.Fprintln(os.Stderr, "remote: warning: runner cancellation failed:", cancelErr)
			} else {
				fmt.Fprintln(os.Stderr, "remote: runner cancelled")
			}
			cancel()
		}
		return fail(err)
	}
	res, err := unpackResult(root, resultPath, replyID, cfg.Outputs)
	if err != nil {
		return fail(err)
	}
	if err := gh.deleteReleaseAndTag(ctx, rel.ID, jobID); err != nil {
		fmt.Fprintln(os.Stderr, "remote: warning: remote cleanup failed:", err)
		cleanup = false
	} else {
		cleanup = false
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "remote worker:", res.Error)
	}
	return res.ExitCode
}

func fail(err error) int { fmt.Fprintln(os.Stderr, "remote:", err); return 1 }

func gitRoot(cwd string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	b, err := cmd.Output()
	if err != nil {
		return "", errors.New("must be run inside a git working tree")
	}
	return strings.TrimSpace(string(b)), nil
}

func loadConfig(root string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(root, ".remote.yml"))
	if err != nil {
		return c, fmt.Errorf("read .remote.yml: %w", err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Repository == "" || strings.Count(c.Repository, "/") != 1 {
		return c, errors.New("repository must be owner/name")
	}
	if c.Recipient == "" {
		return c, errors.New("recipient is required")
	}
	for _, p := range append(append([]string{}, c.Outputs...), c.Exclude...) {
		if !safePattern(p) {
			return c, fmt.Errorf("unsafe path pattern %q", p)
		}
	}
	return c, nil
}

func safePattern(p string) bool {
	p = filepath.ToSlash(p)
	return p != "" && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "../") && !strings.Contains(p, "/../") && !filepath.IsAbs(p)
}

func authToken(ctx context.Context) (string, error) {
	if v := os.Getenv("GH_TOKEN"); v != "" {
		return v, nil
	}
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		return v, nil
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	b, err := cmd.Output()
	if err != nil {
		return "", errors.New("GitHub auth required: set GH_TOKEN/GITHUB_TOKEN or run gh auth login")
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("gh returned an empty token")
	}
	return v, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

func makeRequest(ctx context.Context, root, out string, meta Request, excludes []string, recipient age.Recipient) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := age.Encrypt(f, recipient)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	mb, _ := json.Marshal(meta)
	if err := writeBytes(tw, "meta.json", mb, 0600); err != nil {
		return err
	}
	files, err := selectedFiles(ctx, root, excludes)
	if err != nil {
		return err
	}
	for _, rel := range files {
		if err := addFile(tw, root, rel, "workspace/"+filepath.ToSlash(rel)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return enc.Close()
}

func selectedFiles(ctx context.Context, root string, excludes []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "-co", "--exclude-standard", "-z")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	defaults := []string{".git/**", "node_modules/**", "target/**", ".gradle/**", "build/**", "dist/**", ".remote-state.json"}
	excludes = append(defaults, excludes...)
	seen := map[string]bool{}
	var files []string
	for _, raw := range strings.Split(string(b), "\x00") {
		if raw == "" {
			continue
		}
		rel := filepath.Clean(raw)
		excluded := false
		for _, p := range excludes {
			ok, _ := doublestar.Match(filepath.ToSlash(p), filepath.ToSlash(rel))
			if ok {
				excluded = true
				break
			}
		}
		if excluded || seen[rel] {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing workspace symlink %q", rel)
		}
		if info.Mode().IsRegular() {
			seen[rel] = true
			files = append(files, rel)
		}
	}
	sort.Strings(files)
	return files, nil
}

func addFile(tw *tar.Writer, root, rel, name string) error {
	p := filepath.Join(root, rel)
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", rel)
	}
	h := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}

func writeBytes(tw *tar.Writer, name string, b []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func worker(args []string) error {
	if len(args) == 0 {
		return errors.New("worker phase is required")
	}
	phase := args[0]
	args = args[1:]
	var requestPath, resultPath, dir string
	for i := 0; i < len(args); i++ {
		if i+1 < len(args) {
			switch args[i] {
			case "--request":
				requestPath = args[i+1]
				i++
			case "--result":
				resultPath = args[i+1]
				i++
			case "--dir":
				dir = args[i+1]
				i++
			}
		}
	}
	if dir == "" {
		return errors.New("worker directory is required")
	}
	switch phase {
	case "prepare":
		if requestPath == "" {
			return errors.New("request path is required")
		}
		identityText := os.Getenv("REMOTE_AGE_IDENTITY")
		if identityText == "" {
			return errors.New("worker identity is not configured")
		}
		identity, err := age.ParseX25519Identity(strings.TrimSpace(identityText))
		if err != nil {
			return errors.New("invalid worker identity")
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		meta, err := unpackRequest(requestPath, dir, identity)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(meta)
		return os.WriteFile(filepath.Join(dir, "meta.json"), b, 0600)
	case "execute":
		meta, err := readWorkerMeta(dir)
		if err != nil {
			return err
		}
		exitCode, runErr := execute(dir, meta, filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr"))
		b, _ := json.Marshal(Result{ExitCode: exitCode, Error: runErr})
		return os.WriteFile(filepath.Join(dir, "result.json"), b, 0600)
	case "pack":
		if resultPath == "" {
			return errors.New("result path is required")
		}
		meta, err := readWorkerMeta(dir)
		if err != nil {
			return err
		}
		var res Result
		b, err := os.ReadFile(filepath.Join(dir, "result.json"))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &res); err != nil {
			return err
		}
		return packResult(resultPath, dir, meta, res)
	default:
		return errors.New("invalid worker phase")
	}
}

func readWorkerMeta(dir string) (Request, error) {
	var meta Request
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return meta, err
	}
	err = json.Unmarshal(b, &meta)
	return meta, err
}

func unpackRequest(path, tmp string, identity age.Identity) (Request, error) {
	var meta Request
	f, err := os.Open(path)
	if err != nil {
		return meta, err
	}
	defer f.Close()
	r, err := age.Decrypt(f, identity)
	if err != nil {
		return meta, err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return meta, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return meta, err
		}
		if h.Typeflag != tar.TypeReg {
			return meta, errors.New("request contains non-regular entry")
		}
		if h.Name == "meta.json" {
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&meta); err != nil {
				return meta, err
			}
			continue
		}
		if !strings.HasPrefix(h.Name, "workspace/") {
			return meta, errors.New("invalid request path")
		}
		rel := strings.TrimPrefix(h.Name, "workspace/")
		dst, err := safeJoin(filepath.Join(tmp, "workspace"), rel)
		if err != nil {
			return meta, err
		}
		if err := writeTarFile(dst, tr, h, 100<<30); err != nil {
			return meta, err
		}
	}
	if len(meta.Argv) == 0 {
		return meta, errors.New("empty command")
	}
	if _, err := age.ParseX25519Recipient(meta.ReplyTo); err != nil {
		return meta, errors.New("invalid reply recipient")
	}
	return meta, nil
}

func execute(tmp string, meta Request, stdoutPath, stderrPath string) (int, string) {
	wd, err := safeJoin(filepath.Join(tmp, "workspace"), meta.WorkDir)
	if err != nil {
		return 1, err.Error()
	}
	if st, err := os.Stat(wd); err != nil || !st.IsDir() {
		return 1, "working directory does not exist"
	}
	out, err := os.Create(stdoutPath)
	if err != nil {
		return 1, err.Error()
	}
	defer out.Close()
	eout, err := os.Create(stderrPath)
	if err != nil {
		return 1, err.Error()
	}
	defer eout.Close()
	cmd := exec.Command(meta.Argv[0], meta.Argv[1:]...)
	cmd.Dir = wd
	cmd.Stdout = out
	cmd.Stderr = eout
	cmd.Env = os.Environ()
	err = cmd.Run()
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), ""
	}
	return 1, err.Error()
}

func packResult(path, tmp string, meta Request, res Result) error {
	recipient, err := age.ParseX25519Recipient(meta.ReplyTo)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := age.Encrypt(f, recipient)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	b, _ := json.Marshal(res)
	if err := writeBytes(tw, "result.json", b, 0600); err != nil {
		return err
	}
	for _, n := range []string{"stdout", "stderr"} {
		if err := addFile(tw, tmp, n, n); err != nil {
			return err
		}
	}
	workspace := filepath.Join(tmp, "workspace")
	seen := map[string]bool{}
	for _, pattern := range meta.Outputs {
		matches, err := doublestar.FilepathGlob(filepath.Join(workspace, filepath.FromSlash(pattern)))
		if err != nil {
			return err
		}
		for _, p := range matches {
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			rel, err := filepath.Rel(workspace, p)
			if err != nil || !safePattern(filepath.ToSlash(rel)) {
				return errors.New("unsafe output path")
			}
			if seen[rel] {
				continue
			}
			seen[rel] = true
			if err := addFile(tw, workspace, rel, "outputs/"+filepath.ToSlash(rel)); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return enc.Close()
}

func unpackResult(root, path string, identity age.Identity, patterns []string) (Result, error) {
	var res Result
	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	r, err := age.Decrypt(f, identity)
	if err != nil {
		return res, err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return res, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, err
		}
		if h.Typeflag != tar.TypeReg {
			return res, errors.New("result contains non-regular entry")
		}
		switch h.Name {
		case "result.json":
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&res); err != nil {
				return res, err
			}
		case "stdout":
			if _, err := io.Copy(os.Stdout, tr); err != nil {
				return res, err
			}
		case "stderr":
			if _, err := io.Copy(os.Stderr, tr); err != nil {
				return res, err
			}
		default:
			if !strings.HasPrefix(h.Name, "outputs/") {
				return res, errors.New("invalid result entry")
			}
			rel := strings.TrimPrefix(h.Name, "outputs/")
			if !matchesAny(rel, patterns) {
				return res, fmt.Errorf("worker returned unconfigured output %q", rel)
			}
			dst, err := safeJoin(root, rel)
			if err != nil {
				return res, err
			}
			if err := writeTarFileAtomic(dst, tr, h, 100<<30); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func matchesAny(path string, patterns []string) bool {
	for _, p := range patterns {
		ok, _ := doublestar.Match(filepath.ToSlash(p), filepath.ToSlash(path))
		if ok {
			return true
		}
	}
	return false
}
func safeJoin(root, name string) (string, error) {
	name = filepath.FromSlash(name)
	if name == "" || filepath.IsAbs(name) {
		return "", errors.New("unsafe archive path")
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("archive path traversal")
	}
	dst := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, dst)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("archive path escapes root")
	}
	return dst, nil
}
func writeTarFile(dst string, r io.Reader, h *tar.Header, limit int64) error {
	if h.Size < 0 || h.Size > limit {
		return errors.New("archive entry too large")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if err := rejectSymlinkParents(dst); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
	if err != nil {
		return err
	}
	n, e := io.CopyN(f, r, h.Size)
	ce := f.Close()
	if e != nil {
		return e
	}
	if n != h.Size {
		return io.ErrUnexpectedEOF
	}
	return ce
}
func writeTarFileAtomic(dst string, r io.Reader, h *tar.Header, limit int64) error {
	if h.Size < 0 || h.Size > limit {
		return errors.New("output too large")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := rejectSymlinkParents(dst); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".remote-output-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(os.FileMode(h.Mode) & 0777); err != nil {
		tmp.Close()
		return err
	}
	n, e := io.CopyN(tmp, r, h.Size)
	if e == nil && n != h.Size {
		e = io.ErrUnexpectedEOF
	}
	if ce := tmp.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if st, err := os.Lstat(dst); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to replace symlink")
	}
	return os.Rename(name, dst)
}
func rejectSymlinkParents(path string) error {
	p := filepath.Dir(path)
	for {
		st, err := os.Lstat(p)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in destination path")
		}
		next := filepath.Dir(p)
		if next == p {
			break
		}
		p = next
	}
	return nil
}
func mustRel(root, cwd string) string {
	r, err := filepath.Rel(root, cwd)
	if err != nil {
		return "."
	}
	return filepath.ToSlash(r)
}

func (g *githubClient) api(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GitHub API %s: %s", resp.Status, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
func (g *githubClient) createRelease(ctx context.Context, id string) (release, error) {
	var r release
	b, _ := json.Marshal(map[string]any{"tag_name": "remote-job-" + id, "name": "remote job " + id, "prerelease": true})
	err := g.api(ctx, "POST", "/repos/"+g.repo+"/releases", strings.NewReader(string(b)), &r)
	return r, err
}
func (g *githubClient) upload(ctx context.Context, uploadURL, name, path string) error {
	uploadURL = strings.Split(uploadURL, "{")[0] + "?name=" + url.QueryEscape(name)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, f)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	if st, err := f.Stat(); err == nil {
		req.ContentLength = st.Size()
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("asset upload: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
func (g *githubClient) dispatch(ctx context.Context, id string) error {
	b, _ := json.Marshal(map[string]any{"ref": "main", "inputs": map[string]string{"job_id": id}})
	return g.api(ctx, "POST", "/repos/"+g.repo+"/actions/workflows/worker.yml/dispatches", strings.NewReader(string(b)), nil)
}
func (g *githubClient) waitResult(ctx context.Context, id, out string) error {
	tag := "remote-job-" + id
	delay := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		var r release
		if err := g.api(ctx, "GET", "/repos/"+g.repo+"/releases/tags/"+tag, nil, &r); err != nil {
			return err
		}
		for _, a := range r.Assets {
			if a.Name == "result.age" {
				return g.downloadAsset(ctx, a.ID, out)
			}
		}
		if delay < 15*time.Second {
			delay += 2 * time.Second
		}
	}
}
func (g *githubClient) downloadAsset(ctx context.Context, id int64, out string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", g.repo, id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("result download: %s", resp.Status)
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, e := io.Copy(f, resp.Body)
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}
func (g *githubClient) deleteReleaseAndTag(ctx context.Context, releaseID int64, id string) error {
	if err := g.api(ctx, "DELETE", fmt.Sprintf("/repos/%s/releases/%d", g.repo, releaseID), nil, nil); err != nil {
		return err
	}
	return g.api(ctx, "DELETE", "/repos/"+g.repo+"/git/refs/tags/remote-job-"+id, nil, nil)
}

func (g *githubClient) cancelJob(ctx context.Context, jobID string) error {
	for {
		var runs struct {
			WorkflowRuns []struct {
				ID           int64  `json:"id"`
				DisplayTitle string `json:"display_title"`
				Status       string `json:"status"`
			} `json:"workflow_runs"`
		}
		path := "/repos/" + g.repo + "/actions/runs?event=workflow_dispatch&per_page=50"
		if err := g.api(ctx, "GET", path, nil, &runs); err != nil {
			return err
		}
		for _, run := range runs.WorkflowRuns {
			if run.DisplayTitle == "remote-"+jobID && run.Status != "completed" {
				return g.api(ctx, "POST", fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", g.repo, run.ID), nil, nil)
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("matching active workflow run not found")
		case <-time.After(2 * time.Second):
		}
	}
}

var _ = bufio.ErrInvalidUnreadByte
var _ = runtime.GOOS
