package server

// diskalerts.go: a full disk stops backups, builds and databases, so Dash reads
// each server's filesystems and raises one incident per filesystem. A server
// that cannot be read proves nothing, so its rules are left as they are.

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/incident"
	"github.com/useteploy/teploy-dash/internal/remote"
	sshclient "github.com/useteploy/teploy-dash/internal/ssh"
)

const (
	diskWarnPercent     = 90
	diskCriticalPercent = 95
	// Between this and diskWarnPercent an open incident stays open, so a disk
	// hovering at the threshold does not alert over and over.
	diskRecoverPercent = 85
	diskSweepInterval  = 10 * time.Minute
	diskReadTimeout    = 30 * time.Second
	diskSweepWorkers   = 4
	// Pseudo and read-only filesystems are always "full" or say nothing useful.
	diskReadCommand = "df -kP -x tmpfs -x devtmpfs -x squashfs -x overlay -x iso9660 -x efivarfs"
)

// Two reads, so a build that briefly fills a disk does not page anyone.
var diskPolicy = incident.Policy{FailAfter: 2, RecoverAfter: 1}

type diskUsage struct {
	Mount       string
	UsedPercent int
	FreeKB      int64
}

// parseDiskUsage reads `df -kP` output. A filesystem listed twice (a bind
// mount) counts once, under the first mount shown.
func parseDiskUsage(out string) ([]diskUsage, error) {
	var disks []diskUsage
	seen := map[string]bool{}
	sawHeader := false
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "Filesystem" {
			sawHeader = true
			continue
		}
		if len(f) < 6 {
			return nil, fmt.Errorf("short df line %q", line)
		}
		if f[4] == "-" {
			continue // a filesystem with no size: nothing to judge
		}
		pct, err := strconv.Atoi(strings.TrimSuffix(f[4], "%"))
		if err != nil {
			return nil, fmt.Errorf("df capacity in %q: %w", line, err)
		}
		free, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("df free space in %q: %w", line, err)
		}
		if seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		disks = append(disks, diskUsage{Mount: strings.Join(f[5:], " "), UsedPercent: pct, FreeKB: free})
	}
	if !sawHeader {
		return nil, fmt.Errorf("not df output: %q", strings.TrimSpace(out))
	}
	return disks, nil
}

func diskRule(server, mount string, percent int) incident.Rule {
	severity := alert.SeverityWarning
	if percent >= diskCriticalPercent {
		severity = alert.SeverityCritical
	}
	return incident.Rule{Key: "disk:" + server + ":" + mount, Source: alert.SourceDisk, ID: "disk-" + server + "-" + mount,
		Name: "Disk " + mount + " on " + server, Server: server, Target: server + ":" + mount, Severity: severity,
		Path: "/", Policy: diskPolicy}
}

func formatFreeKB(kb int64) string {
	if gb := float64(kb) / (1 << 20); gb >= 1 {
		return fmt.Sprintf("%.0f GB", gb)
	}
	return fmt.Sprintf("%d MB", kb>>10)
}

// observeDisks applies one complete read of one server.
func (s *Server) observeDisks(server string, disks []diskUsage, now time.Time) {
	t := s.alerts.tracker
	if t == nil {
		return
	}
	present := map[string]bool{}
	for _, d := range disks {
		rule := diskRule(server, d.Mount, d.UsedPercent)
		present[rule.Key] = true
		msg := fmt.Sprintf("%s on %s is %d%% full, %s free.", d.Mount, server, d.UsedPercent, formatFreeKB(d.FreeKB))
		switch {
		case d.UsedPercent >= diskWarnPercent:
			t.Observe(rule, false, now, msg)
		case d.UsedPercent < diskRecoverPercent:
			t.Observe(rule, true, now, msg)
		}
	}
	for _, inc := range t.Open() {
		if inc.Source == alert.SourceDisk && inc.Server == server && !present[inc.Key] {
			t.Retire(inc.Key, "this filesystem is no longer on the server")
		}
	}
}

// retireRemovedDiskServers closes incidents for servers no longer in the fleet.
func (s *Server) retireRemovedDiskServers(servers []remote.ServerConn) {
	t := s.alerts.tracker
	if t == nil {
		return
	}
	known := map[string]bool{}
	for _, srv := range servers {
		known[srv.Name] = true
	}
	for _, inc := range t.Open() {
		if inc.Source == alert.SourceDisk && !known[inc.Server] {
			t.Retire(inc.Key, "this server was removed")
		}
	}
}

func readServerDisks(ctx context.Context, srv remote.ServerConn) (string, error) {
	c, err := sshclient.Connect(ctx, srv.Host, srv.User, srv.KeyPath)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Run(ctx, diskReadCommand)
}

func (s *Server) sweepDisks(ctx context.Context) {
	servers, err := s.resolveServers(ctx)
	if err != nil {
		log.Printf("[disk] server discovery: %v", err)
		return
	}
	read := s.alerts.diskRead
	if read == nil {
		read = readServerDisks
	}
	sem := make(chan struct{}, diskSweepWorkers)
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		sem <- struct{}{}
		go func(srv remote.ServerConn) {
			defer wg.Done()
			defer func() { <-sem }()
			readCtx, cancel := context.WithTimeout(ctx, diskReadTimeout)
			defer cancel()
			out, err := read(readCtx, srv)
			if err != nil {
				log.Printf("[disk] reading %s: %v", srv.Name, err)
				return
			}
			disks, err := parseDiskUsage(out)
			if err != nil {
				log.Printf("[disk] reading %s: %v", srv.Name, err)
				return
			}
			s.observeDisks(srv.Name, disks, time.Now())
		}(srv)
	}
	wg.Wait()
	s.retireRemovedDiskServers(servers)
}

// RunDiskWatch sweeps until ctx ends.
func (s *Server) RunDiskWatch(ctx context.Context) {
	if s.alerts.tracker == nil {
		return
	}
	t := time.NewTicker(diskSweepInterval)
	defer t.Stop()
	for {
		s.sweepDisks(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
