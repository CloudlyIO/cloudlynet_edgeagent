package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloudlynet_edgeagent/testsuite/loggen"
)

const (
	retOUI    = "8C1F64"
	retSerial = "2205610001"
	// Another fleet device behind the same FTP root; never ours to delete.
	retOtherSerial = "2205610002"
)

// uploadNames returns the Log and ErrorLog names loggen would produce for a
// device at time at, so the retention parser is tested against the real
// filename generator rather than a hand-copied layout.
func uploadNames(oui, serial string, at time.Time) (string, string) {
	c := loggen.Config{OUI: oui, Serial: serial, At: at}
	return c.LogName(), c.ErrorLogName()
}

func TestUploadTimeParsesLoggenNames(t *testing.T) {
	at := time.Date(2026, 10, 27, 9, 41, 0, 0, time.FixedZone("", 8*3600))
	logName, errName := uploadNames(retOUI, retSerial, at)
	for _, n := range []string{logName, errName} {
		ts, ok := uploadTime(n, retOUI, retSerial)
		if !ok {
			t.Fatalf("uploadTime(%q) not recognised", n)
		}
		if !ts.Equal(at) {
			t.Fatalf("uploadTime(%q) = %s, want %s", n, ts, at)
		}
	}
}

func TestExpiredUploadsSelectsOnlyThisDevicesOldUploads(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	keep := 6 * time.Hour
	oldLog, oldErr := uploadNames(retOUI, retSerial, now.Add(-7*time.Hour))
	// Same instant as oldLog but stamped in +0800: still 7h old, so the
	// timezone in the name must be honoured, not ignored.
	oldLocal, _ := uploadNames(retOUI, retSerial, now.Add(-7*time.Hour).In(time.FixedZone("", 8*3600)))
	recentLog, recentErr := uploadNames(retOUI, retSerial, now.Add(-5*time.Hour))
	otherOld, _ := uploadNames(retOUI, retOtherSerial, now.Add(-7*time.Hour))
	otherOUIOld, _ := uploadNames("AABBCC", retSerial, now.Add(-7*time.Hour))
	names := []string{
		oldLog, oldErr, oldLocal, recentLog, recentErr, otherOld, otherOUIOld,
		retOUI + "_" + retSerial + "_PowerOn_20240622_142356_fsm.log.gz", // forensic: not a periodic upload
		"Log_garbage_" + retOUI + "." + retSerial + ".gz",                // unparseable stamp
		"uploads",          // the write-protected dir ftp-init stages
		"archive-pre-fix",  // an operator's parking subdirectory
		"Log_" + retSerial, // truncated name
	}

	got := expiredUploads(names, retOUI, retSerial, now, keep)
	sort.Strings(got)
	want := []string{oldErr, oldLocal, oldLog}
	sort.Strings(want)
	if oldLocal == oldLog {
		t.Fatalf("test setup: local-tz name should differ from the UTC one")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expiredUploads =\n  %q\nwant\n  %q", got, want)
	}
}

func TestExpiredUploadsKeepsEverythingWhenRetentionIsOff(t *testing.T) {
	now := time.Now()
	ancient, _ := uploadNames(retOUI, retSerial, now.Add(-90*24*time.Hour))
	for _, keep := range []time.Duration{0, -time.Hour} {
		if got := expiredUploads([]string{ancient}, retOUI, retSerial, now, keep); len(got) != 0 {
			t.Fatalf("keep=%s: expiredUploads = %q, want none", keep, got)
		}
	}
}

func TestRetentionConfigDefaultsEnvAndConf(t *testing.T) {
	t.Setenv("NANOLINK_FTP_RETENTION", "")
	cfg := defaultNanolinkConfig().withEnvOverrides()
	if cfg.FTP.Retention != defaultFTPRetention {
		t.Fatalf("default retention = %s, want %s", cfg.FTP.Retention, defaultFTPRetention)
	}

	t.Setenv("NANOLINK_FTP_RETENTION", "90m")
	if got := defaultNanolinkConfig().withEnvOverrides().FTP.Retention; got != 90*time.Minute {
		t.Fatalf("env 90m -> %s", got)
	}
	t.Setenv("NANOLINK_FTP_RETENTION", "0")
	if got := defaultNanolinkConfig().withEnvOverrides().FTP.Retention; got != 0 {
		t.Fatalf("env 0 -> %s, want 0 (keep everything)", got)
	}
	t.Setenv("NANOLINK_FTP_RETENTION", "six hours")
	if got := defaultNanolinkConfig().withEnvOverrides().FTP.Retention; got != defaultFTPRetention {
		t.Fatalf("invalid env -> %s, want the default kept", got)
	}

	t.Setenv("NANOLINK_FTP_RETENTION", "")
	fromConf, err := loadNanolinkConfig(writeConf(t, "ftp:\n  retention: 2h\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fromConf.FTP.Retention != 2*time.Hour {
		t.Fatalf("conf retention = %s, want 2h", fromConf.FTP.Retention)
	}
	offConf, err := loadNanolinkConfig(writeConf(t, "ftp:\n  retention: 0s\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if offConf.FTP.Retention != 0 {
		t.Fatalf("conf retention 0s = %s, want 0", offConf.FTP.Retention)
	}
}

// TestPruneOwnUploadsDeletesOverFTP drives the real curl command lines against
// an in-process FTP server: only this device's expired uploads are DELEted,
// and the delete call opens no data connection (no NLST/LIST after the DELEs).
func TestPruneOwnUploadsDeletesOverFTP(t *testing.T) {
	now := time.Now().UTC()
	oldLog, oldErr := uploadNames(retOUI, retSerial, now.Add(-8*time.Hour))
	recentLog, _ := uploadNames(retOUI, retSerial, now.Add(-time.Hour))
	otherOld, _ := uploadNames(retOUI, retOtherSerial, now.Add(-8*time.Hour))
	srv := newStubFTP(t, oldLog, oldErr, recentLog, otherOld)

	dev := &device{oui: retOUI, serial: retSerial}
	pruneOwnUploads(dev, ftpUploadConfig{host: srv.host(), user: "nybsys", pass: "x", retention: 6 * time.Hour}, now)

	want := []string{otherOld, recentLog}
	sort.Strings(want)
	if got := srv.names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("files after prune = %q, want %q", got, want)
	}
	cmds := srv.commands()
	lastNLST, firstDELE := -1, -1
	for i, c := range cmds {
		if c == "NLST" || strings.HasPrefix(c, "NLST ") || strings.HasPrefix(c, "LIST") {
			lastNLST = i
		}
		if strings.HasPrefix(c, "DELE ") && firstDELE < 0 {
			firstDELE = i
		}
	}
	if firstDELE < 0 || lastNLST > firstDELE {
		t.Fatalf("want one listing then DELEs with no listing after; commands = %q", cmds)
	}
}

// TestCurlDeleteBatchesAndSurvivesMissingFiles: a backlog larger than one
// curl invocation is fully deleted, and a name that is already gone does not
// stop the rest of its batch.
func TestCurlDeleteBatchesAndSurvivesMissingFiles(t *testing.T) {
	var names []string
	for i := 0; i < 2*deleteBatch+17; i++ {
		names = append(names, fmt.Sprintf("Log_20260101.%04d+0000_%s.%s.gz", i, retOUI, retSerial))
	}
	srv := newStubFTP(t, names...)
	toDelete := append([]string{"Log_already_gone.gz"}, names...)
	if err := curlDelete(srv.host(), "nybsys", "x", toDelete); err != nil {
		t.Fatalf("curlDelete: %v", err)
	}
	if left := srv.names(); len(left) != 0 {
		t.Fatalf("%d file(s) left after delete, want 0: %q", len(left), left[:min(5, len(left))])
	}
}
