package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// The tools here read access and hardening settings, so the assistant can
// name the actual accounts, shares and services when it explains a
// security audit finding. Every projection is an allowlist: TrueNAS returns
// password hashes, private keys and SSH host keys in some of these
// responses, and none of it can reach the model.

// maxUsers bounds list_users on servers with a great many accounts.
const maxUsers = 200

// localUsersPath asks for the NAS's own accounts. user.query also returns
// directory-service (AD/LDAP) accounts, which aren't the NAS's to manage
// and can run to thousands.
const localUsersPath = `/user?filters=[["local","=",true]]`

// ----- list_users -------------------------------------------------------------

func listUsers(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_users",
			Description: "List local user accounts and their access: whether each is locked or " +
				"has a password, can log in over SSH with a password or a key, and what sudo " +
				"rights it has. Includes root; leaves out other built-in system accounts. Use " +
				"when the user asks about accounts, admin or root access, sudo, or SSH logins.",
			Parameters: noArgs,
		},
		StatusLine: "Checking user accounts…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			raw, err := tn.GetRaw(ctx, localUsersPath)
			if err != nil {
				return "", err
			}
			var users []map[string]any
			if err := json.Unmarshal(raw, &users); err != nil {
				return "", fmt.Errorf("decode users: %w", err)
			}
			listed, total := projectUsers(users)
			out := map[string]any{"users": listed, "total": total}
			if total > len(listed) {
				out["truncated"] = true
				out["note"] = fmt.Sprintf("Only the first %d of %d accounts are listed.", len(listed), total)
			}
			return toJSON(out)
		},
	}
}

// projectUsers keeps what matters for access questions, for at most
// maxUsers accounts, and counts them all. An SSH key is reported only as
// present or not.
func projectUsers(users []map[string]any) (listed []map[string]any, total int) {
	listed = []map[string]any{}
	for _, u := range users {
		builtin, _ := u["builtin"].(bool)
		name, _ := u["username"].(string)
		if builtin && name != "root" {
			continue
		}
		total++
		if len(listed) == maxUsers {
			continue
		}
		entry := map[string]any{}
		copyFields(entry, u,
			"username", "full_name", "uid", "builtin", "locked", "password_disabled",
			"ssh_password_enabled", "sudo_commands", "sudo_commands_nopasswd", "smb",
			"shell", "twofactor_auth_configured", "roles")
		key, _ := u["sshpubkey"].(string)
		entry["has_ssh_key"] = strings.TrimSpace(key) != ""
		listed = append(listed, entry)
	}
	return listed, total
}

// ----- security_settings --------------------------------------------------------

func securitySettings(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "security_settings",
			Description: "Get the server's security-related settings: SSH (port, password " +
				"logins and the groups allowed them, TCP forwarding, weak ciphers), FTP (TLS, who " +
				"can log in, anonymous access), which services are running and start at boot, the " +
				"web UI's HTTPS redirect and ports, and certificate expiry. Use for questions about " +
				"SSH, FTP, services, HTTPS, certificates, or hardening the server.",
			Parameters: noArgs,
		},
		StatusLine: "Checking security settings…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			out := map[string]any{}
			if ssh := getObject(ctx, tn, "/ssh"); ssh != nil {
				// Password logins need passwordauth plus either the
				// account's own switch (list_users) or one of these groups.
				out["ssh"] = pick(ssh, "tcpport", "passwordauth", "password_login_groups",
					"kerberosauth", "tcpfwd", "weak_ciphers", "bindiface")
			}
			if ftp := getObject(ctx, tn, "/ftp"); ftp != nil {
				out["ftp"] = projectFTP(ftp)
			}
			if services := getList(ctx, tn, "/service"); services != nil {
				out["services"] = projectServices(services)
			}
			if general := getObject(ctx, tn, "/system/general"); general != nil {
				ui := pick(general, "ui_httpsredirect", "ui_port", "ui_httpsport", "ui_certificate_name")
				if cert, ok := general["ui_certificate"].(map[string]any); ok {
					ui["ui_certificate_name"] = cert["name"]
				}
				out["web_ui"] = ui
			}
			if certs := getList(ctx, tn, "/certificate"); certs != nil {
				list := make([]map[string]any, 0, len(certs))
				for _, c := range certs {
					list = append(list, projectCertificate(c))
				}
				out["certificates"] = list
			}
			if len(out) == 0 {
				return "", errors.New("couldn't read any security settings from TrueNAS")
			}
			return toJSON(out)
		},
	}
}

// projectServices reports each service as running or not and whether it
// starts at boot, instead of TrueNAS's raw state strings.
func projectServices(services []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(services))
	for _, s := range services {
		state, _ := s["state"].(string)
		out = append(out, map[string]any{
			"name":          s["service"],
			"running":       strings.EqualFold(state, "RUNNING"),
			"start_on_boot": s["enable"],
		})
	}
	return out
}

// projectFTP says who can log in rather than passing on TrueNAS's option
// names, which don't mean what they say. From TrueNAS's proftpd template:
// "onlyanonymous" with an anonymous path adds anonymous logins, and either
// option lets every local user log in; with neither, only the ftp group can.
func projectFTP(ftp map[string]any) map[string]any {
	entry := pick(ftp, "port", "tls", "tls_policy")
	onlyAnonymous, _ := ftp["onlyanonymous"].(bool)
	onlyLocal, _ := ftp["onlylocal"].(bool)
	anonPath, _ := ftp["anonpath"].(string)
	entry["anonymous_login"] = onlyAnonymous && strings.TrimSpace(anonPath) != ""
	if onlyLocal || onlyAnonymous {
		entry["local_user_login"] = "every local user"
	} else {
		entry["local_user_login"] = "only members of the ftp group"
	}
	return entry
}

// projectCertificate keeps a certificate's name, expiry and issuer. On
// TrueNAS 25.04 a certificate signed by an internal CA gives that whole CA
// as its issuer, private key included, so only the CA's name is kept.
func projectCertificate(c map[string]any) map[string]any {
	entry := pick(c, "name", "expired", "until")
	switch issuer := c["issuer"].(type) {
	case string:
		entry["issuer"] = issuer
	case map[string]any:
		if name, ok := issuer["name"].(string); ok {
			entry["issuer"] = name
		}
	}
	return entry
}

// ----- share projections (list_shares) -----------------------------------------

// projectSMBShare keeps an SMB share's identity and its access settings.
// TrueNAS 25.10 moved guest access and the recycle bin into purpose-specific
// options, and only legacy shares have them; both shapes are read.
func projectSMBShare(s map[string]any) map[string]any {
	entry := map[string]any{"type": "smb"}
	copyFields(entry, s, "name", "path", "enabled", "comment", "purpose", "locked",
		"readonly", "ro", "browsable")
	// "abe" before 25.10.
	if v, ok := firstPresent("access_based_share_enumeration", s); ok {
		entry["access_based_share_enumeration"] = v
	} else if v, ok := firstPresent("abe", s); ok {
		entry["access_based_share_enumeration"] = v
	}
	opts, _ := s["options"].(map[string]any)
	if v, ok := firstPresent("guestok", s, opts); ok {
		entry["guest_access"] = v
	}
	if v, ok := firstPresent("recyclebin", s, opts); ok {
		entry["recycle_bin"] = v
	}
	if audit, ok := s["audit"].(map[string]any); ok {
		entry["audit_logging"] = audit["enable"]
	}
	return entry
}

// projectNFSShare keeps an NFS export's path and who can reach it as whom.
func projectNFSShare(s map[string]any) map[string]any {
	entry := map[string]any{"type": "nfs"}
	copyFields(entry, s, "path", "enabled", "comment", "hosts", "networks", "locked",
		"ro", "maproot_user", "maproot_group", "mapall_user", "mapall_group", "security")
	return entry
}

// ----- helpers ----------------------------------------------------------------

func pick(src map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	copyFields(out, src, keys...)
	return out
}

func firstPresent(key string, sources ...map[string]any) (any, bool) {
	for _, src := range sources {
		if v, ok := src[key]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

// getObject and getList read one settings object or list, or nil when
// TrueNAS doesn't have it (an older version, a service it doesn't run).
// security_settings reports whatever it could read.
func getObject(ctx context.Context, tn *truenas.Client, path string) map[string]any {
	raw, err := tn.GetRaw(ctx, path)
	if err != nil {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

func getList(ctx context.Context, tn *truenas.Client, path string) []map[string]any {
	raw, err := tn.GetRaw(ctx, path)
	if err != nil {
		return nil
	}
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	return list
}
