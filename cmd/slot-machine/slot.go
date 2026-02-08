package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type slot struct {
	name    string // directory basename, e.g. "slot-abc1234"
	commit  string
	dir     string // absolute path
	cmd     *exec.Cmd
	done    chan struct{}
	alive   bool
	appPort int // dynamic
	intPort int // dynamic
}

func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

func (o *orchestrator) runSetup(dir string, appPort, intPort int) error {
	cmd := exec.Command("/bin/sh", "-c", o.cfg.SetupCommand)
	cmd.Dir = dir
	cmd.Env = o.buildEnv(appPort, intPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (o *orchestrator) buildEnv(appPort, intPort int) []string {
	env := os.Environ()
	if o.cfg.EnvFile != "" {
		envPath := o.cfg.EnvFile
		if !filepath.IsAbs(envPath) {
			envPath = filepath.Join(o.repoDir, envPath)
		}
		if extra, err := loadEnvFile(envPath); err == nil {
			env = append(env, extra...)
		}
	}
	env = append(env,
		"SLOT_MACHINE=1",
		fmt.Sprintf("PORT=%d", appPort),
		fmt.Sprintf("INTERNAL_PORT=%d", intPort),
	)
	if o.authSecret != "" {
		env = append(env, "SLOT_MACHINE_AUTH_SECRET="+o.authSecret)
	}
	return env
}

func (o *orchestrator) startProcess(dir, commit string, appPort, intPort int) (*slot, error) {
	cmd := exec.Command("/bin/sh", "-c", o.cfg.StartCommand)
	cmd.Dir = dir
	cmd.Env = o.buildEnv(appPort, intPort)
	logPath := filepath.Join(o.dataDir, fmt.Sprintf("%s.log", filepath.Base(dir)))
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &slot{
		name:    filepath.Base(dir),
		commit:  commit,
		dir:     dir,
		cmd:     cmd,
		done:    make(chan struct{}),
		alive:   true,
		appPort: appPort,
		intPort: intPort,
	}

	go func() {
		cmd.Wait()
		o.mu.Lock()
		s.alive = false
		if o.liveSlot == s {
			o.appProxy.clearTarget()
			o.intProxy.clearTarget()
		}
		o.mu.Unlock()
		close(s.done)
	}()

	return s, nil
}

func (o *orchestrator) drainAll() {
	o.mu.Lock()
	var slots []*slot
	if o.liveSlot != nil {
		slots = append(slots, o.liveSlot)
	}
	if o.prevSlot != nil && o.prevSlot.cmd != nil {
		slots = append(slots, o.prevSlot)
	}
	o.mu.Unlock()
	for _, s := range slots {
		o.drain(s)
	}
}

func (o *orchestrator) drain(s *slot) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}

	syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)

	select {
	case <-s.done:
	case <-time.After(time.Duration(o.cfg.DrainTimeoutMs) * time.Millisecond):
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

func (o *orchestrator) healthCheck(s *slot) bool {
	timeout := time.Duration(o.cfg.HealthTimeoutMs) * time.Millisecond
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.intPort, o.cfg.HealthEndpoint)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return false
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
