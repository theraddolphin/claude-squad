package git

import (
	"bufio"
	"claude-squad/config"
	"claude-squad/log"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LinkedReposFile lists other repos a session can get a worktree of. It lives at the root of
// the repo cs is launched from; its presence makes every session a multi-repo session.
// One repo per line:
//
//	<alias> <path> [auto]
//
// Repos are linked on demand (`cs link <alias>` from inside a session); `auto` creates the
// worktree up front for every session. The path may be absolute or relative to the repo
// root. Lines starting with # are ignored.
const LinkedReposFile = ".claude-squad-repos"

// LinkedState is the serializable state of one linked worktree.
type LinkedState struct {
	Alias            string `json:"alias"`
	RepoPath         string `json:"repo_path"`
	WorktreePath     string `json:"worktree_path"`
	BaseCommitSHA    string `json:"base_commit_sha"`
	IsExistingBranch bool   `json:"is_existing_branch"`
}

type linkedRepo struct {
	alias, path string
	auto        bool
}

// readLinkedRepos parses LinkedReposFile in repoRoot; no file means no linked repos.
func readLinkedRepos(repoRoot string) ([]linkedRepo, error) {
	f, err := os.Open(filepath.Join(repoRoot, LinkedReposFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", LinkedReposFile, err)
	}
	defer f.Close()

	var repos []linkedRepo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields) > 3 || (len(fields) == 3 && fields[2] != "auto") {
			return nil, fmt.Errorf("invalid line in %s: %q (want '<alias> <path> [auto]')", LinkedReposFile, line)
		}
		path := fields[1]
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoRoot, path)
		}
		root, err := findGitRepoRoot(path)
		if err != nil {
			return nil, fmt.Errorf("linked repo %s: %w", fields[0], err)
		}
		repos = append(repos, linkedRepo{alias: fields[0], path: root, auto: len(fields) == 3})
	}
	return repos, scanner.Err()
}

// attachLinked registers (but does not set up) worktrees for the `auto` repos in
// LinkedReposFile; the rest are linked on demand with Link.
func (g *GitWorktree) attachLinked() error {
	repos, err := readLinkedRepos(g.repoPath)
	if err != nil {
		return err
	}
	if g.isMulti() {
		if trashRoot, err := trashDir(); err == nil {
			go emptyTrash(trashRoot) // leftovers from a cs that quit mid-delete
		}
	}
	for _, r := range repos {
		if r.auto {
			g.linked = append(g.linked, g.newLinked(r))
		}
	}
	return nil
}

func (g *GitWorktree) newLinked(r linkedRepo) *GitWorktree {
	return &GitWorktree{
		repoPath:     r.path,
		worktreePath: g.worktreePath + "." + r.alias,
		sessionName:  g.sessionName,
		branchName:   g.branchName,
		alias:        r.alias,
	}
}

// isMulti reports whether this is a multi-repo session (the launch repo has LinkedReposFile),
// even before any repo is linked.
func (g *GitWorktree) isMulti() bool {
	if g.alias != "" {
		return false
	}
	_, err := os.Stat(filepath.Join(g.repoPath, LinkedReposFile))
	return err == nil
}

// markerDir holds one <alias>.json per linked worktree. It is how a running cs learns about
// repos linked from inside the session (`cs link`), and it survives pause.
func (g *GitWorktree) markerDir() string { return g.worktreePath + ".linked" }

func (g *GitWorktree) writeMarker(l *GitWorktree) {
	if err := os.MkdirAll(g.markerDir(), 0755); err != nil {
		log.ErrorLog.Printf("linked marker: %v", err)
		return
	}
	data, _ := json.Marshal(l.state())
	if err := os.WriteFile(filepath.Join(g.markerDir(), l.alias+".json"), data, 0644); err != nil {
		log.ErrorLog.Printf("linked marker: %v", err)
	}
}

// links returns the linked worktrees, first picking up any linked from outside this process.
func (g *GitWorktree) links() []*GitWorktree {
	g.linkedMu.Lock()
	defer g.linkedMu.Unlock()
	if entries, err := os.ReadDir(g.markerDir()); err == nil {
		known := make(map[string]bool)
		for _, l := range g.linked { // already holding linkedMu: do not call links() here
			known[l.alias] = true
		}
		for _, e := range entries {
			alias := strings.TrimSuffix(e.Name(), ".json")
			if known[alias] || alias == e.Name() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(g.markerDir(), e.Name()))
			var st LinkedState
			if err != nil || json.Unmarshal(data, &st) != nil {
				continue
			}
			g.linked = append(g.linked, g.fromState(st))
		}
	}
	return append([]*GitWorktree(nil), g.linked...)
}

func (l *GitWorktree) state() LinkedState {
	return LinkedState{
		Alias:            l.alias,
		RepoPath:         l.repoPath,
		WorktreePath:     l.worktreePath,
		BaseCommitSHA:    l.baseCommitSHA,
		IsExistingBranch: l.isExistingBranch,
	}
}

func (g *GitWorktree) fromState(s LinkedState) *GitWorktree {
	return &GitWorktree{
		repoPath:         s.RepoPath,
		worktreePath:     s.WorktreePath,
		sessionName:      g.sessionName,
		branchName:       g.branchName,
		baseCommitSHA:    s.BaseCommitSHA,
		isExistingBranch: s.IsExistingBranch,
		alias:            s.Alias,
	}
}

// Link adds a worktree of the LinkedReposFile repo `alias` to the session, on the session
// branch, exposed as ./<alias>. Linking an already linked repo just makes sure it is there.
// It returns the path inside the session.
func (g *GitWorktree) Link(alias string) (string, error) {
	repos, err := readLinkedRepos(g.repoPath)
	if err != nil {
		return "", err
	}
	var repo *linkedRepo
	var names []string
	for i := range repos {
		names = append(names, repos[i].alias)
		if repos[i].alias == alias {
			repo = &repos[i]
		}
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo %q; available: %s", alias, strings.Join(names, ", "))
	}
	var l *GitWorktree
	for _, existing := range g.links() {
		if existing.alias == alias {
			l = existing
		}
	}
	if l == nil {
		l = g.newLinked(*repo)
	}
	if err := g.attach(l); err != nil {
		return "", err
	}
	g.linkedMu.Lock()
	found := false
	for _, existing := range g.linked {
		found = found || existing.alias == alias
	}
	if !found {
		g.linked = append(g.linked, l)
	}
	g.linkedMu.Unlock()
	g.writeSlotRecord()
	g.runHook("setup") // so per-repo config (env files etc.) is written for the new repo
	return filepath.Join(g.worktreePath, alias), nil
}

// attach sets up a linked worktree, symlinks it into the session and records its marker.
func (g *GitWorktree) attach(l *GitWorktree) error {
	if err := l.setupLinked(); err != nil {
		return fmt.Errorf("linked repo %s: %w", l.alias, err)
	}
	link := filepath.Join(g.worktreePath, l.alias)
	_ = os.Remove(link)
	if err := os.Symlink(l.worktreePath, link); err != nil {
		return fmt.Errorf("failed to link %s into session: %w", l.alias, err)
	}
	g.writeMarker(l)
	return nil
}

// LinkedAliases lists the available repos and whether each is linked in this session.
func (g *GitWorktree) LinkedAliases() (available []string, linked map[string]bool, err error) {
	repos, err := readLinkedRepos(g.repoPath)
	if err != nil {
		return nil, nil, err
	}
	linked = make(map[string]bool)
	for _, l := range g.links() {
		linked[l.alias] = true
	}
	for _, r := range repos {
		available = append(available, r.alias)
	}
	return available, linked, nil
}

// SearchBranchesAll is SearchBranches over the launch repo and its linked repos, so an
// existing api or web branch can be picked even if the launch repo has never seen it.
// Like SearchBranches, results are most recently committed first, across all repos.
func SearchBranchesAll(dir, filter string) ([]string, error) {
	root, err := findGitRepoRoot(dir)
	if err != nil {
		return nil, err
	}
	repos, err := readLinkedRepos(root)
	if err != nil || len(repos) == 0 {
		return SearchBranches(root, filter)
	}
	// One `git branch` per repo, in parallel: sequentially it adds up (~1.3s for 13 repos).
	paths := append([]string{root}, linkedPaths(repos)...)
	outs := make([][]byte, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, path := range paths {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			outs[i], errs[i] = exec.Command("git", "-C", path, "branch", "-a",
				"--format=%(committerdate:unix) %(refname:short)").CombinedOutput()
		}(i, path)
	}
	wg.Wait()
	latest := make(map[string]int64) // branch -> newest commit time in any repo
	lower := strings.ToLower(filter)
	for i, out := range outs {
		if errs[i] != nil {
			return nil, fmt.Errorf("failed to list branches in %s: %s (%w)", paths[i], out, errs[i])
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			ts, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok || strings.Contains(ref, "HEAD") {
				continue
			}
			name := strings.TrimPrefix(ref, "origin/")
			if filter != "" && !strings.Contains(strings.ToLower(name), lower) {
				continue
			}
			if t, _ := strconv.ParseInt(ts, 10, 64); t > latest[name] || latest[name] == 0 {
				latest[name] = t
			}
		}
	}
	all := make([]string, 0, len(latest))
	for name := range latest {
		all = append(all, name)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if latest[all[i]] != latest[all[j]] {
			return latest[all[i]] > latest[all[j]]
		}
		return all[i] < all[j]
	})
	if len(all) > MaxBranchSearchResults {
		all = all[:MaxBranchSearchResults]
	}
	return all, nil
}

func linkedPaths(repos []linkedRepo) []string {
	var paths []string
	for _, r := range repos {
		paths = append(paths, r.path)
	}
	return paths
}

// LinkedStates returns the serializable state of the linked worktrees.
func (g *GitWorktree) LinkedStates() []LinkedState {
	var states []LinkedState
	for _, l := range g.links() {
		states = append(states, l.state())
	}
	return states
}

// Slot returns the session's port slot (0 if none).
func (g *GitWorktree) Slot() int { return g.slot }

// SetSlot restores the port slot from storage.
func (g *GitWorktree) SetSlot(slot int) { g.slot = slot }

// SetLinkedStates restores linked worktrees from storage.
func (g *GitWorktree) SetLinkedStates(states []LinkedState) {
	g.linkedMu.Lock()
	defer g.linkedMu.Unlock()
	g.linked = nil
	for _, s := range states {
		g.linked = append(g.linked, g.fromState(s))
	}
}

// Setup creates the session worktree and one worktree per linked repo, each symlinked
// into the session worktree as ./<alias>.
func (g *GitWorktree) Setup() error {
	// With linked repos, Setup also runs on resume when only one side is missing,
	// so never recreate a worktree that is still intact (it would drop uncommitted work).
	multi := g.isMulti()
	if valid, _ := g.IsValidWorktreeSelf(); !valid || !multi {
		// A branch picked from a linked repo may not exist in the launch repo yet;
		// create it there (and own it, so kill removes it) instead of failing.
		if g.isExistingBranch && multi && !g.branchExists() {
			g.isExistingBranch = false
		}
		if err := g.SetupSelf(); err != nil {
			return err
		}
	}
	for _, l := range g.links() {
		if err := g.attach(l); err != nil {
			return err
		}
	}
	if !multi {
		return nil
	}
	if err := g.claimSlot(); err != nil {
		return err
	}
	g.writeSlotRecord()
	g.runHook("setup")
	return nil
}

// setupLinked creates a linked worktree on the session branch. New branches start
// from the repo's default branch (origin/HEAD), not whatever the main checkout is on.
func (l *GitWorktree) setupLinked() error {
	if valid, _ := l.IsValidWorktreeSelf(); valid {
		return nil
	}
	_, _ = l.runGitCommand(l.repoPath, "worktree", "remove", "-f", l.worktreePath)
	_ = os.RemoveAll(l.worktreePath)

	firstSetup := l.baseCommitSHA == ""
	if l.branchExists() {
		// A branch we didn't create is never deleted on kill.
		if firstSetup {
			l.isExistingBranch = true
		}
		base := l.baseCommitSHA
		if err := l.setupFromExistingBranch(); err != nil {
			return err
		}
		// Keep the original base across pause/resume so the diff still shows earlier work.
		if base != "" {
			l.baseCommitSHA = base
		}
		return nil
	}

	start := l.defaultRef()
	if remote, branch, ok := strings.Cut(start, "/"); ok && remote == "origin" {
		_, _ = l.runGitCommand(l.repoPath, "fetch", "origin", branch) // best-effort, may be offline
	}
	if _, err := l.runGitCommand(l.repoPath, "worktree", "add", "-b", l.branchName, l.worktreePath, start); err != nil {
		return fmt.Errorf("failed to create worktree from %s: %w", start, err)
	}
	return l.recordBaseCommit()
}

func (g *GitWorktree) branchExists() bool {
	_, localErr := g.runGitCommand(g.repoPath, "show-ref", "--verify", "refs/heads/"+g.branchName)
	_, remoteErr := g.runGitCommand(g.repoPath, "show-ref", "--verify", "refs/remotes/origin/"+g.branchName)
	return localErr == nil || remoteErr == nil
}

// defaultRef returns the remote default branch (e.g. origin/main), or HEAD if unknown.
func (l *GitWorktree) defaultRef() string {
	out, err := l.runGitCommand(l.repoPath, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err != nil {
		return "HEAD"
	}
	return strings.TrimSpace(out)
}

// Cleanup removes all worktrees and the branches this session created.
func (g *GitWorktree) Cleanup() error {
	var errs []error
	multi := g.isMulti()
	links := g.links()
	if multi {
		g.runHook("teardown")
		g.releaseSlot()
	}
	for _, l := range links {
		_ = os.Remove(filepath.Join(g.worktreePath, l.alias))
		if err := l.removeFast(); err != nil {
			errs = append(errs, fmt.Errorf("linked repo %s: %w", l.alias, err))
		}
		if err := l.CleanupSelf(); err != nil {
			errs = append(errs, fmt.Errorf("linked repo %s: %w", l.alias, err))
		}
	}
	if err := g.CleanupSelf(); err != nil {
		errs = append(errs, err)
	}
	if multi {
		for _, hook := range []string{"setup", "pause", "teardown"} {
			_ = os.Remove(g.worktreePath + "." + hook + ".log")
		}
		_ = os.RemoveAll(g.markerDir())
	}
	if len(errs) > 0 {
		return g.combineErrors(errs)
	}
	return nil
}

// Remove removes all worktrees but keeps their branches (pause). The pause hook runs first
// so it can set aside untracked state worth keeping, like dependency folders.
func (g *GitWorktree) Remove() error {
	if g.isMulti() {
		g.runHook("pause")
	}
	for _, l := range g.links() {
		_ = os.Remove(filepath.Join(g.worktreePath, l.alias))
		if err := l.removeFast(); err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
	}
	return g.RemoveSelf()
}

// removeFast removes a linked worktree without blocking on deleting its files: linked
// worktrees hold big trees like node_modules, and `git worktree remove` deletes them inline
// (pause and kill froze the UI ~10s). The folder is moved to trash, git forgets it, and the
// files are deleted in the background. Callers commit first; uncommitted work is dropped,
// same as `git worktree remove -f`.
func (l *GitWorktree) removeFast() error {
	if _, err := os.Stat(l.worktreePath); os.IsNotExist(err) {
		return nil
	}
	trashRoot, err := trashDir()
	if err != nil {
		return l.RemoveSelf()
	}
	trash := filepath.Join(trashRoot, fmt.Sprintf("%s_%d", filepath.Base(l.worktreePath), time.Now().UnixNano()))
	if err := os.Rename(l.worktreePath, trash); err != nil {
		return l.RemoveSelf()
	}
	if err := l.PruneSelf(); err != nil {
		return err
	}
	go emptyTrash(trashRoot)
	return nil
}

func trashDir() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "trash")
	return dir, os.MkdirAll(dir, 0755)
}

// emptyTrash deletes everything in trash, including leftovers from a cs that exited mid-delete.
func emptyTrash(trashRoot string) {
	entries, _ := os.ReadDir(trashRoot)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(trashRoot, e.Name()))
	}
}

// Prune prunes worktree metadata in every repo.
func (g *GitWorktree) Prune() error {
	for _, l := range g.links() {
		if err := l.PruneSelf(); err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
	}
	return g.PruneSelf()
}

// IsValidWorktree reports whether every worktree of the session is intact.
func (g *GitWorktree) IsValidWorktree() (bool, error) {
	for _, l := range g.links() {
		if valid, err := l.IsValidWorktreeSelf(); err != nil || !valid {
			return valid, err
		}
	}
	return g.IsValidWorktreeSelf()
}

// IsBranchCheckedOut reports whether the session branch is checked out in any main checkout.
func (g *GitWorktree) IsBranchCheckedOut() (bool, error) {
	for _, l := range g.links() {
		if checked, err := l.IsBranchCheckedOutSelf(); err != nil || checked {
			return checked, err
		}
	}
	return g.IsBranchCheckedOutSelf()
}

// IsDirty reports whether any worktree of the session has uncommitted changes.
func (g *GitWorktree) IsDirty() (bool, error) {
	for _, l := range g.links() {
		if dirty, err := l.IsDirtySelf(); err != nil || dirty {
			return dirty, err
		}
	}
	return g.IsDirtySelf()
}

// CommitChanges commits uncommitted changes in every worktree.
func (g *GitWorktree) CommitChanges(commitMessage string) error {
	for _, l := range g.links() {
		if err := l.CommitChangesSelf(commitMessage); err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
	}
	return g.CommitChangesSelf(commitMessage)
}

// PushChanges commits and pushes every worktree that has work on the branch. Linked repos
// with nothing beyond their default branch are skipped, as is a session repo with no remote.
func (g *GitWorktree) PushChanges(commitMessage string, open bool) error {
	if !g.isMulti() {
		return g.PushChangesSelf(commitMessage, open)
	}
	if err := g.CommitChanges(commitMessage); err != nil {
		return err
	}
	pushed := 0
	for _, l := range g.links() {
		ok, err := l.pushLinked(open)
		if err != nil {
			return fmt.Errorf("linked repo %s: %w", l.alias, err)
		}
		if ok {
			pushed++
		}
	}
	if _, err := g.runGitCommand(g.worktreePath, "remote", "get-url", "origin"); err == nil {
		return g.PushChangesSelf(commitMessage, open)
	}
	if pushed == 0 {
		return fmt.Errorf("nothing to push: no repo has commits on %s", g.branchName)
	}
	return nil
}

// pushLinked pushes the branch if it has commits beyond the default branch.
func (l *GitWorktree) pushLinked(open bool) (bool, error) {
	out, err := l.runGitCommand(l.worktreePath, "rev-list", "--count", l.defaultRef()+"..HEAD")
	if err != nil {
		return false, err
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(out)); n == 0 {
		return false, nil
	}
	cmd := exec.Command("git", "push", "-u", "origin", l.branchName)
	cmd.Dir = l.worktreePath
	if output, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("failed to push branch: %s (%w)", output, err)
	}
	if open {
		if err := l.OpenBranchURL(); err != nil {
			log.ErrorLog.Printf("failed to open branch URL: %v", err)
		}
	}
	return true, nil
}

// Diff returns one diff covering the session repo and every linked repo, with linked
// paths prefixed by their alias (e.g. api/squadhealth/models.py).
func (g *GitWorktree) Diff() *DiffStats {
	stats := g.DiffSelf()
	for _, l := range g.links() {
		ls := l.linkedDiff(false)
		mergeStats(stats, ls)
	}
	return stats
}

// DiffNumstat returns line counts across the session repo and every linked repo.
func (g *GitWorktree) DiffNumstat() *DiffStats {
	stats := g.DiffNumstatSelf()
	for _, l := range g.links() {
		mergeStats(stats, l.linkedDiff(true))
	}
	return stats
}

func (l *GitWorktree) linkedDiff(numstat bool) *DiffStats {
	stats := &DiffStats{}
	if _, err := l.runGitCommand(l.worktreePath, "add", "-N", "."); err != nil {
		stats.Error = err
		return stats
	}
	args := []string{"--no-pager", "diff",
		"--src-prefix=a/" + l.alias + "/", "--dst-prefix=b/" + l.alias + "/"}
	if numstat {
		args = append(args, "--numstat")
	}
	out, err := l.runGitCommand(l.worktreePath, append(args, l.baseCommitSHA)...)
	if err != nil {
		stats.Error = err
		return stats
	}
	if numstat {
		stats.Added, stats.Removed = parseNumstat(out)
		return stats
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			stats.Added++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			stats.Removed++
		}
	}
	stats.Content = out
	return stats
}

func mergeStats(into, from *DiffStats) {
	into.Added += from.Added
	into.Removed += from.Removed
	into.Content += from.Content
	if into.Error == nil {
		into.Error = from.Error
	}
}
