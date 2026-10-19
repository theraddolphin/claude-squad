package git

import (
	"claude-squad/config"
	"claude-squad/log"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// HooksDir holds optional executable hooks in the repo cs is launched from:
//
//	setup     runs after every session worktree is created or recreated (new and resume)
//	teardown  runs when a session is killed, before its worktrees are removed
//
// Hooks get CS_SESSION, CS_BRANCH, CS_SLOT, CS_SESSION_DIR, and for each linked repo
// CS_REPO_<ALIAS> (its session worktree) and CS_MAIN_<ALIAS> (its main checkout).
// Output goes to <session dir>.<hook>.log. A failing hook is logged, never fatal.
const HooksDir = ".claude-squad"

const maxSlots = 99

func slotsDir() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "slots"), nil
}

// claimSlot gives the session the lowest free port slot, keeping any slot it already has.
func (g *GitWorktree) claimSlot() error {
	if g.slot > 0 {
		return nil
	}
	dir, err := slotsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for n := 1; n <= maxSlots; n++ {
		f, err := os.OpenFile(filepath.Join(dir, strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			continue
		}
		_, _ = f.WriteString(g.worktreePath + "\n")
		_ = f.Close()
		g.slot = n
		return nil
	}
	return fmt.Errorf("no free port slot (all %d in use, see %s)", maxSlots, dir)
}

func (g *GitWorktree) releaseSlot() {
	if g.slot == 0 {
		return
	}
	if dir, err := slotsDir(); err == nil {
		_ = os.Remove(filepath.Join(dir, strconv.Itoa(g.slot)))
	}
	g.slot = 0
}

func (g *GitWorktree) runHook(name string) {
	script := filepath.Join(g.repoPath, HooksDir, name)
	if _, err := os.Stat(script); err != nil {
		return
	}
	env := append(os.Environ(),
		"CS_SESSION="+g.sessionName,
		"CS_BRANCH="+g.branchName,
		"CS_SLOT="+strconv.Itoa(g.slot),
		"CS_SESSION_DIR="+g.worktreePath,
	)
	for _, l := range g.linked {
		key := strings.ToUpper(strings.ReplaceAll(l.alias, "-", "_"))
		env = append(env, "CS_REPO_"+key+"="+l.worktreePath, "CS_MAIN_"+key+"="+l.repoPath)
	}
	logPath := g.worktreePath + "." + name + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		log.ErrorLog.Printf("hook %s: %v", name, err)
		return
	}
	defer logFile.Close()
	cmd := exec.Command(script)
	cmd.Dir = g.repoPath
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		log.ErrorLog.Printf("hook %s failed (see %s): %v", name, logPath, err)
	}
}
