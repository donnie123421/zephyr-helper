package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectUsersKeepsAccessAndDropsSecrets(t *testing.T) {
	users := []map[string]any{
		{"username": "root", "uid": 0, "builtin": true, "locked": false, "password_disabled": true,
			"ssh_password_enabled": false, "sshpubkey": "ssh-ed25519 AAAAC3Nza root@laptop",
			"unixhash": "$6$secret", "smbhash": "*SECRET*"},
		{"username": "daemon", "uid": 1, "builtin": true},
		{"username": "bob", "uid": 3000, "builtin": false, "sudo_commands_nopasswd": []any{"ALL"},
			"ssh_password_enabled": true, "sshpubkey": nil, "unixhash": "$6$bobsecret"},
	}
	got := projectUsers(users)
	if len(got) != 2 || got[0]["username"] != "root" || got[1]["username"] != "bob" {
		t.Fatalf("users = %v, want root and bob only", got)
	}
	if got[0]["has_ssh_key"] != true || got[1]["has_ssh_key"] != false {
		t.Errorf("has_ssh_key = %v / %v", got[0]["has_ssh_key"], got[1]["has_ssh_key"])
	}
	data, _ := json.Marshal(got)
	for _, secret := range []string{"secret", "SECRET", "AAAAC3Nza", "sshpubkey", "unixhash", "smbhash"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("projection leaked %q: %s", secret, data)
		}
	}
}

func TestProjectUsersIsBounded(t *testing.T) {
	var users []map[string]any
	for i := 0; i < maxUsers+50; i++ {
		users = append(users, map[string]any{"username": "u", "builtin": false})
	}
	if got := projectUsers(users); len(got) != maxUsers {
		t.Errorf("got %d users, want %d", len(got), maxUsers)
	}
}

func TestProjectSMBShareReadsBothShapes(t *testing.T) {
	// TrueNAS 25.10+: guest access and the recycle bin live in the options
	// of a legacy share.
	modern := projectSMBShare(map[string]any{
		"name": "public", "path": "/mnt/tank/public", "enabled": true, "purpose": "LEGACY_SHARE",
		"readonly": false, "audit": map[string]any{"enable": true, "watch_list": []any{}},
		"options": map[string]any{"purpose": "LEGACY_SHARE", "guestok": true, "recyclebin": false},
	})
	if modern["guest_access"] != true || modern["recycle_bin"] != false || modern["audit_logging"] != true {
		t.Errorf("25.10 share = %v", modern)
	}

	// A default 25.10 share has neither setting.
	plain := projectSMBShare(map[string]any{"name": "docs", "purpose": "DEFAULT_SHARE",
		"options": map[string]any{"purpose": "DEFAULT_SHARE", "aapl_name_mangling": false}})
	if _, ok := plain["guest_access"]; ok {
		t.Errorf("default share shouldn't report guest access: %v", plain)
	}

	// Before 25.10 both sat at the top level.
	legacy := projectSMBShare(map[string]any{"name": "old", "guestok": false, "recyclebin": true, "ro": true})
	if legacy["guest_access"] != false || legacy["recycle_bin"] != true || legacy["ro"] != true {
		t.Errorf("pre-25.10 share = %v", legacy)
	}
}

func TestProjectNFSShareKeepsAccessControl(t *testing.T) {
	got := projectNFSShare(map[string]any{
		"path": "/mnt/tank/media", "enabled": true, "hosts": []any{}, "networks": []any{"10.0.0.0/24"},
		"maproot_user": "root", "mapall_user": nil, "ro": false, "security": []any{"SYS"}, "id": 4,
	})
	if got["maproot_user"] != "root" || got["type"] != "nfs" {
		t.Errorf("nfs = %v", got)
	}
	if _, ok := got["id"]; ok {
		t.Errorf("projection passed through a field it doesn't list: %v", got)
	}
}

func TestSettingsProjectionsAreAllowlists(t *testing.T) {
	ssh := pick(map[string]any{
		"tcpport": 22, "passwordauth": true, "host_rsa_key": "-----BEGIN RSA PRIVATE KEY-----",
		"host_ed25519_key": "-----BEGIN OPENSSH PRIVATE KEY-----",
	}, "tcpport", "passwordauth", "rootlogin")
	if len(ssh) != 2 || ssh["passwordauth"] != true {
		t.Errorf("ssh = %v", ssh)
	}
	cert := pick(map[string]any{"name": "truenas_default", "expired": false, "until": "Mon Jan 1 2035",
		"privatekey": "-----BEGIN PRIVATE KEY-----"}, "name", "expired", "until", "issuer")
	if _, ok := cert["privatekey"]; ok {
		t.Errorf("certificate projection leaked the private key: %v", cert)
	}
}

func TestProjectServices(t *testing.T) {
	got := projectServices([]map[string]any{
		{"service": "ssh", "state": "RUNNING", "enable": true},
		{"service": "ftp", "state": "STOPPED", "enable": false},
	})
	if got[0]["running"] != true || got[1]["running"] != false || got[1]["start_on_boot"] != false {
		t.Errorf("services = %v", got)
	}
}
