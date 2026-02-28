package ttydiscover

import (
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type Candidate struct {
	PID     int
	TTY     string
	Command string
	Args    string
}

func List() ([]Candidate, error) {
	if _, err := exec.LookPath("ps"); err != nil {
		return nil, errors.New("ps not found in PATH")
	}
	cmd := exec.Command("ps", "-eo", "pid=,tty=,comm=,args=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	candidates := parsePSOutput(string(out))
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].TTY == candidates[j].TTY {
			return candidates[i].PID < candidates[j].PID
		}
		return candidates[i].TTY < candidates[j].TTY
	})
	return candidates, nil
}

func parsePSOutput(raw string) []Candidate {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	candidates := make([]Candidate, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		tty := strings.TrimSpace(fields[1])
		if tty == "" || tty == "?" || tty == "-" {
			continue
		}
		cmd := strings.TrimSpace(fields[2])
		args := ""
		if len(fields) > 3 {
			args = strings.Join(fields[3:], " ")
		}
		candidates = append(candidates, Candidate{
			PID:     pid,
			TTY:     ttyPath(tty),
			Command: cmd,
			Args:    args,
		})
	}
	return candidates
}

func ttyPath(tty string) string {
	tty = strings.TrimSpace(tty)
	if tty == "" || strings.HasPrefix(tty, "/") {
		return tty
	}
	return "/dev/" + tty
}
