// zcalld-pluginctl keeps installers and JS runtimes out of the native CLI core.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LIghtJUNction/zcalld-compat/builtin"
	"github.com/LIghtJUNction/zcalld-compat/community"
	"github.com/LIghtJUNction/zcalld-compat/internal/buildinfo"
	"github.com/LIghtJUNction/zcalld-compat/routing"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.String("zcalld-pluginctl"))
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 1 && args[0] == "routing-worker" {
		return routing.Serve(os.Stdin, os.Stdout)
	}
	if len(args) == 1 && args[0] == "routing-validate" {
		return routing.Validate(os.Stdin, os.Stdout)
	}
	var root, revision, hash, name, version, file, source, privacyRoot, modelRoot, provider, label, account, owners, expectedPlan string
	var desktopInbox string
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--json":
		case "--desktop-inbox", "--root", "--revision", "--sha256", "--name", "--version", "--file", "--input-file", "--source", "--privacy-root", "--model-root", "--to-magpie", "--provider", "--label", "--account", "--owners", "--expect-plan":
			if i+1 == len(args) {
				return errors.New("missing option value")
			}
			i++
			switch a {
			case "--desktop-inbox":
				desktopInbox = args[i]
			case "--expect-plan":
				expectedPlan = args[i]
			case "--provider":
				provider = args[i]
			case "--label":
				label = args[i]
			case "--owners":
				owners = args[i]
			case "--account":
				account = args[i]
			case "--root":
				root = args[i]
			case "--revision":
				revision = args[i]
			case "--sha256":
				hash = args[i]
			case "--name":
				name = args[i]
			case "--version":
				version = args[i]
			case "--file", "--input-file":
				file = args[i]
			case "--source", "--to-magpie":
				source = args[i]
			case "--privacy-root":
				privacyRoot = args[i]
			case "--model-root":
				modelRoot = args[i]
			}
		default:
			if strings.HasPrefix(a, "--") {
				return errors.New("unknown plugin option")
			}
			positional = append(positional, a)
		}
	}
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("--root must be an absolute extension directory")
	}
	if len(positional) == 0 {
		positional = []string{"list"}
	}
	var result any
	var err error
	switch positional[0] {
	case "maintenance":
		if len(positional) == 2 && positional[1] == "worker" {
			if provider != "" || file != "" || account != "" {
				return errors.New("maintenance worker takes only --root")
			}
			return runMaintenanceWorker(root, source, expectedPlan, desktopInbox)
		}
		result, err = runMaintenance(root, source, expectedPlan, desktopInbox, provider, file, account, positional[1:])
	case "quota":
		if len(positional) != 2 {
			return errors.New("quota expects list or refresh --account ID")
		}
		switch positional[1] {
		case "list":
			if account != "" {
				return errors.New("quota list takes no account")
			}
			result, err = community.QuotaList(root)
		case "refresh":
			if account == "" {
				return errors.New("quota refresh requires --account")
			}
			result, err = community.QuotaRefresh(root, account)
		default:
			return errors.New("quota expects list or refresh --account ID")
		}
	case "import-mcp":
		result, err = community.ImportMCP(root, source)
	case "codex":
		if len(positional) >= 2 && positional[1] == "policy" {
			if len(positional) != 3 || (positional[2] != "plan" && positional[2] != "sync") || source == "" {
				return errors.New("codex policy expects plan --source PATH or sync --source PATH --expect-plan HASH")
			}
			result, err = builtin.CodexPolicy(root, source, expectedPlan, positional[2] == "sync")
			break
		}
		action := "show"
		if len(positional) > 2 {
			return errors.New("codex takes one action")
		}
		if len(positional) == 2 {
			action = positional[1]
		}
		result, err = community.CodexHTTP(root, action, file)
	case "mcp":
		action := "show"
		if len(positional) > 2 {
			return errors.New("mcp takes one action")
		}
		if len(positional) == 2 {
			action = positional[1]
		}
		result, err = community.MCP(root, action)
	case "handoff":
		result, err = builtin.Handoff(root, account, owners)
	case "accounts":
		if len(positional) == 2 && positional[1] == "session" {
			return community.Session(root, os.Stdin, os.Stdout)
		}
		if len(positional) == 2 && positional[1] == "handoff" {
			result, err = builtin.Handoff(root, account, owners)
		} else {
			result, err = community.Accounts(root, positional[1:], community.AccountOptions{Provider: provider, Label: label, ID: account, File: file})
		}
	case "import-builtin":
		result, err = builtin.Import(root, source)
		if err == nil {
			err = community.ReconcileAccounts(root)
		}
	case "sync-auth":
		result, err = builtin.Sync(root, source)
	case "import-magpie":
		result, err = community.ImportMagpie(root, community.ImportOptions{Source: source, PrivacyRoot: privacyRoot, ModelRoot: modelRoot})
	case "session":
		return community.Session(root, os.Stdin, os.Stdout)
	case "verify":
		if len(positional) != 2 {
			return errors.New("verify needs a name")
		}
		result, err = community.Verify(root, positional[1])
	case "invoke":
		if len(positional) != 3 {
			return errors.New("invoke needs PROVIDER EVENT")
		}
		result, err = community.Invoke(root, positional[1], positional[2], file)
	case "list":
		if len(positional) != 1 {
			return errors.New("list takes no name")
		}
		result, err = community.List(root)
	case "inspect":
		if len(positional) != 2 {
			return errors.New("inspect needs a name")
		}
		result, err = community.Inspect(root, positional[1])
	case "install":
		if len(positional) != 2 {
			return errors.New("install needs a source")
		}
		result, err = community.Install(root, community.InstallOptions{Source: positional[1], Name: name, Revision: revision, SHA256: hash, Version: version})
	case "update":
		if len(positional) != 2 {
			return errors.New("update needs a name")
		}
		result, err = community.Install(root, community.InstallOptions{Name: positional[1], Revision: revision, SHA256: hash, Version: version, Update: true})
	case "enable", "disable", "remove", "config":
		if len(positional) != 2 {
			return errors.New("plugin action needs a name")
		}
		if positional[0] == "config" && file == "" {
			return errors.New("config needs --file")
		}
		result, err = community.Change(root, positional[1], positional[0], file)
	case "providers":
		result, err = community.Providers(root)
	case "runtime":
		if len(positional) == 2 && positional[1] == "configure" {
			err = community.ConfigureRuntime(root)
			result = map[string]any{"ok": err == nil, "configured": err == nil, "host_config": filepath.Join(root, "host.json"), "restart_required": true}
			break
		}
		if len(positional) != 1 {
			return errors.New("runtime accepts only configure")
		}
		host, pi, e := community.Scripts(root)
		err = e
		result = map[string]any{"host": host, "pi": pi}
	default:
		return errors.New("unknown plugin command")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
