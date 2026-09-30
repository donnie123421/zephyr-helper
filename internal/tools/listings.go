package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// The read-only tools here list what the actions work on, with the names
// and ids the actions take.

// ----- list_backup_tasks -----------------------------------------------------------

func listBackupTasks(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_backup_tasks",
			Description: "List backup tasks: replications, cloud syncs and rsync tasks, with each one's " +
				"kind and id, what it copies and where, whether it's turned on, and how its last run went.",
			Parameters: noArgs,
		},
		StatusLine: "Checking backup tasks…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			now := time.Now()
			out := make([]map[string]any, 0)
			read := 0
			for _, kind := range []string{"replication", "cloud_sync", "rsync"} {
				rows, err := query(ctx, tn, backupKinds[kind].query, nil)
				if err != nil {
					continue
				}
				read++
				for _, task := range rows {
					entry := map[string]any{
						"kind":  kind,
						"name":  taskName(kind, task),
						"route": taskRoute(kind, task),
					}
					copyFields(entry, task, "id", "enabled")
					if last := lastRun(task, now); last != "" {
						entry["last_run"] = last
					}
					if backupRunning(task) {
						entry["running"] = true
					}
					if locked, _ := task["locked"].(bool); locked {
						entry["dataset_locked"] = true
					}
					out = append(out, entry)
				}
			}
			if read == 0 {
				return "", errors.New("couldn't read any backup tasks from TrueNAS")
			}
			return toJSON(map[string]any{"tasks": out})
		},
	}
}

// ----- list_vms / list_containers -----------------------------------------------------

func listMachines(tn *truenas.Client, m machine, name, description, status string) Tool {
	return Tool{
		Def:        Definition{Name: name, Description: description, Parameters: noArgs},
		StatusLine: status,
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			rows, err := query(ctx, tn, m.query, nil)
			if truenas.IsMethodNotFound(err) {
				return toJSON(map[string]any{"note": "This TrueNAS version has no " + m.noun + "s."})
			}
			if err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				entry := map[string]any{}
				copyFields(entry, row, "id", "name", "description", "autostart", "vcpus", "memory")
				if status, ok := row["status"].(map[string]any); ok {
					entry["state"] = status["state"]
				}
				out = append(out, entry)
			}
			return toJSON(map[string]any{m.noun + "s": out})
		},
	}
}

func listVMs(tn *truenas.Client) Tool {
	return listMachines(tn, vmMachine, "list_vms",
		"List virtual machines with their name, state (RUNNING or STOPPED), whether they start at boot, "+
			"and their CPUs and memory (MiB).", "Checking VMs…")
}

func listContainers(tn *truenas.Client) Tool {
	return listMachines(tn, containerMachine, "list_containers",
		"List containers (TrueNAS 26 and later) with their name, state and whether they start at boot.",
		"Checking containers…")
}

// ----- list_services ------------------------------------------------------------------

func listServices(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_services",
			Description: "List TrueNAS services (SMB is cifs) with whether each is running and starts " +
				"at boot.",
			Parameters: noArgs,
		},
		StatusLine: "Checking services…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			rows, err := query(ctx, tn, "service.query", nil)
			if err != nil {
				return "", err
			}
			out := projectServices(rows)
			for _, s := range out {
				if name, _ := s["name"].(string); serviceNames[name] != "" {
					s["label"] = serviceNames[name]
				}
			}
			return toJSON(map[string]any{"services": out})
		},
	}
}
