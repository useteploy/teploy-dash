package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/remote"
)

const dfHeader = "Filesystem 1024-blocks Used Available Capacity Mounted on\n"

func TestParseDiskUsage(t *testing.T) {
	disks, err := parseDiskUsage(dfHeader +
		"/dev/mapper/root 689963008 620000000 34603008 95% /\n" +
		"/dev/mapper/root 689963008 620000000 34603008 95% /var/lib/bind\n" +
		"/dev/sdb1 1000 10 990 1% /mnt/my disk\n" +
		"none - - - - /proc/odd\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("want a bind mount and a sizeless filesystem left out, got %+v", disks)
	}
	if disks[0].Mount != "/" || disks[0].UsedPercent != 95 || disks[0].FreeKB != 34603008 {
		t.Fatalf("root: %+v", disks[0])
	}
	if disks[1].Mount != "/mnt/my disk" {
		t.Fatalf("a mount with a space: %+v", disks[1])
	}
	for _, bad := range []string{"", "sh: df: not found", dfHeader + "/dev/sda1 1 2\n", dfHeader + "/dev/sda1 1 2 x 5% /\n"} {
		if _, err := parseDiskUsage(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func diskRead(s *Server, server string, pct int, at time.Time) {
	s.observeDisks(server, []diskUsage{{Mount: "/", UsedPercent: pct, FreeKB: 33 << 20}}, at)
}

func TestDiskIncident_Lifecycle(t *testing.T) {
	s, sink := alertServer(t)
	writeNotifications(t, `{"channels":[{"id":"hook","type":"webhook","url":"https://h.example"}]}`)
	now := time.Now()
	const key = "disk:infra:/"

	diskRead(s, "infra", 92, now)
	if _, open := openIncidents(t, s)[key]; open {
		t.Fatal("one read must not open an incident: a build can fill a disk briefly")
	}
	diskRead(s, "infra", 80, now.Add(time.Minute))
	diskRead(s, "infra", 92, now.Add(2*time.Minute))
	if _, open := openIncidents(t, s)[key]; open {
		t.Fatal("a read below the threshold resets the count")
	}
	diskRead(s, "infra", 92, now.Add(3*time.Minute))
	msg, open := openIncidents(t, s)[key]
	if !open || !strings.Contains(msg, "/ on infra is 92% full, 33 GB free") {
		t.Fatalf("open=%v msg=%q", open, msg)
	}

	diskRead(s, "infra", 96, now.Add(4*time.Minute))
	for _, inc := range s.alerts.tracker.Open() {
		if inc.Key == key && inc.Severity != alert.SeverityCritical {
			t.Fatalf("95%% and over is critical, got %q", inc.Severity)
		}
	}

	diskRead(s, "infra", 87, now.Add(5*time.Minute))
	if _, open := openIncidents(t, s)[key]; !open {
		t.Fatal("between 85% and 90% an open incident stays open")
	}
	diskRead(s, "infra", 84, now.Add(6*time.Minute))
	if _, open := openIncidents(t, s)[key]; open {
		t.Fatal("under 85% recovers")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(sink.events()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	evs := sink.events()
	if len(evs) == 0 || evs[0].Source != alert.SourceDisk {
		t.Fatalf("want a disk alert sent to the channel, got %+v", evs)
	}
}

func TestDiskSweep_UnreadableServerProvesNothing(t *testing.T) {
	c := &backupCLI{}
	s := newBackupsServer(t, c)
	s.alerts.tracker.EndAdoption()
	now := time.Now()
	const key = "disk:prod:/"
	diskRead(s, "prod", 96, now)
	diskRead(s, "prod", 96, now)
	if _, open := openIncidents(t, s)[key]; !open {
		t.Fatal("setup: incident not open")
	}
	s.alerts.diskRead = func(context.Context, remote.ServerConn) (string, error) {
		return "", errors.New("connection refused")
	}
	s.sweepDisks(context.Background())
	if _, open := openIncidents(t, s)[key]; !open {
		t.Fatal("an unreachable server must not close or retire its incident")
	}
	s.alerts.diskRead = func(context.Context, remote.ServerConn) (string, error) { return "garbage", nil }
	s.sweepDisks(context.Background())
	if _, open := openIncidents(t, s)[key]; !open {
		t.Fatal("an unparseable read must not close its incident")
	}
}

func TestDiskIncident_Retired(t *testing.T) {
	s, _ := alertServer(t)
	now := time.Now()
	for i := 0; i < 2; i++ {
		s.observeDisks("a", []diskUsage{{Mount: "/mnt/x", UsedPercent: 96}, {Mount: "/", UsedPercent: 10}}, now)
		s.observeDisks("b", []diskUsage{{Mount: "/", UsedPercent: 96}}, now)
	}
	s.observeDisks("a", []diskUsage{{Mount: "/", UsedPercent: 10}}, now)
	inc := openIncidents(t, s)
	if _, open := inc["disk:a:/mnt/x"]; open {
		t.Fatal("a filesystem gone from a complete read is retired")
	}
	if _, open := inc["disk:b:/"]; !open {
		t.Fatal("another server's incident must stay")
	}
	s.retireRemovedDiskServers([]remote.ServerConn{{Name: "a"}})
	if _, open := openIncidents(t, s)["disk:b:/"]; open {
		t.Fatal("a removed server's incident is retired")
	}
}
