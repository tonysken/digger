package execution

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type TerraformExecutor interface {
	Init([]string, map[string]string) (string, string, error)
	Apply([]string, *string, map[string]string) (string, string, error)
	Destroy([]string, map[string]string) (string, string, error)
	Plan([]string, map[string]string, string, *string) (bool, string, string, error)
	Show([]string, map[string]string, string, bool) (string, string, error)
}

type Terraform struct {
	WorkingDir string
	Workspace  string
}

func (tf Terraform) Init(params []string, envs map[string]string) (string, string, error) {
	params = append(params, "-input=false")
	params = append(params, "-no-color")
	stdout, stderr, _, err := tf.runTerraformCommand("init", true, envs, nil, params...)

	// switch to workspace for next step
	// TODO: make this an individual and isolated step
	if tf.Workspace != "default" {
		werr := tf.switchToWorkspace(envs)
		if werr != nil {
			slog.Error("Failed to switch workspace",
				"workspace", tf.Workspace,
				"error", werr)
			return "", "", werr
		}
	}

	return stdout, stderr, err
}

func (tf Terraform) Apply(params []string, plan *string, envs map[string]string) (string, string, error) {
	params = append(params, []string{"-lock-timeout=3m"}...)
	params = append(append(append(params, "-input=false"), "-no-color"), "-auto-approve")
	if plan != nil {
		params = append(params, *plan)
	}
	stdout, stderr, _, err := tf.runTerraformCommand("apply", true, envs, nil, params...)
	return stdout, stderr, err
}

func (tf Terraform) Destroy(params []string, envs map[string]string) (string, string, error) {
	params = append(append(append(params, "-input=false"), "-no-color"), "-auto-approve")
	stdout, stderr, _, err := tf.runTerraformCommand("destroy", true, envs, nil, params...)
	return stdout, stderr, err
}

func (tf Terraform) switchToWorkspace(envs map[string]string) error {
	workspaces, _, _, err := tf.runTerraformCommand("workspace", false, envs, nil, "list")
	if err != nil {
		return err
	}
	workspaces = tf.formatTerraformWorkspaces(workspaces)
	if strings.Contains(workspaces, tf.Workspace) {
		slog.Debug("Selecting existing workspace", "workspace", tf.Workspace)
		_, _, _, err := tf.runTerraformCommand("workspace", true, envs, nil, "select", tf.Workspace)
		if err != nil {
			return err
		}
	} else {
		slog.Debug("Creating new workspace", "workspace", tf.Workspace)
		_, _, _, err := tf.runTerraformCommand("workspace", true, envs, nil, "new", tf.Workspace)
		if err != nil {
			return err
		}
	}
	return nil
}

func (tf Terraform) runTerraformCommand(command string, printOutputToStdout bool, envs map[string]string, filterRegex *string, arg ...string) (string, string, int, error) {

	args := []string{command}
	args = append(args, arg...)

	expandedArgs := make([]string, 0)
	for _, p := range args {
		s := os.ExpandEnv(p)
		s = strings.TrimSpace(s)
		slog.Info(fmt.Sprintf("Print Terraform Args : %s=%s", p, s))
		if s != "" {
			expandedArgs = append(expandedArgs, s)
		}
	}

	regEx, err := stringToRegex(filterRegex)
	if err != nil {
		return "", "", 0, err
	}

	var mwout, mwerr io.Writer
	var stdout, stderr bytes.Buffer
	if printOutputToStdout {
		mwout = NewFilteringWriter(os.Stdout, &stdout, regEx)
		mwerr = NewFilteringWriter(os.Stderr, &stderr, regEx)
	} else {
		mwout = NewFilteringWriter(nil, &stdout, regEx)
		mwerr = NewFilteringWriter(nil, &stderr, regEx)
	}

	cmd := exec.Command("terraform", expandedArgs...)
	slog.Info("Running Terraform command",
		slog.Group("command",
			"binary", "terraform",
			"args", RedactSecrets(expandedArgs),
			"workingDir", tf.WorkingDir,
		),
	)
	cmd.Dir = tf.WorkingDir

	env := os.Environ()
	for _, kv := range env {
		slog.Info(fmt.Sprintf("Print Terraform Envs : %s", kv))
	}
	for k, v := range envs {
		slog.Info(fmt.Sprintf("Print Terraform Envs : %s=%s", k, v))
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env
	cmd.Stdout = mwout
	cmd.Stderr = mwerr

	//time.Sleep(300 * time.Second)
	err = cmd.Run()

	// terraform plan can return 2 if there are changes to be applied, so we don't want to fail in that case
	if err != nil && cmd.ProcessState.ExitCode() != 2 {
		slog.Error("Command execution failed",
			"command", "terraform",
			"exitCode", cmd.ProcessState.ExitCode(),
			"error", err,
		)
	}

	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode(), err
}

type StdWriter struct {
	data  []byte
	print bool
}

func (tf Terraform) formatTerraformWorkspaces(list string) string {
	list = strings.TrimSpace(list)
	char_replace := strings.NewReplacer("*", "", "\n", ",", " ", "")
	list = char_replace.Replace(list)
	return list
}

func (tf Terraform) Plan(params []string, envs map[string]string, planArtefactFilePath string, filterRegex *string) (bool, string, string, error) {
	params = append(append(append(params, "-input=false"), "-no-color"), "-detailed-exitcode")
	if planArtefactFilePath != "" {
		params = append(params, []string{"-out", planArtefactFilePath}...)
	}
	params = append(params, "-lock-timeout=3m")
	stdout, stderr, statusCode, err := tf.runTerraformCommand("plan", true, envs, filterRegex, params...)
	if err != nil && statusCode != 2 {
		return false, "", "", err
	}
	return statusCode == 2, stdout, stderr, nil
}

func (tf Terraform) Show(params []string, envs map[string]string, planArtefactFilePath string, returnJson bool) (string, string, error) {
	params = append(params, "-no-color")
	if returnJson {
		params = append(params, "-json")
	}
	params = append(params, planArtefactFilePath)
	stdout, stderr, _, err := tf.runTerraformCommand("show", false, envs, nil, params...)
	if err != nil {
		return "", "", err
	}
	return stdout, stderr, nil
}

func RedactSecret(s string) string {
	exps := []*regexp.Regexp{
		regexp.MustCompile(`\-backend\-config\=access\_key\=(.*)`),
		regexp.MustCompile(`\-backend\-config\=secret\_key\=(.*)`),
		regexp.MustCompile(`\-backend\-config\=token\=(.*)`),
	}
	for _, e := range exps {
		x := e.FindStringSubmatch(s)
		if len(x) > 1 {
			s = strings.ReplaceAll(s, x[1], "<REDACTED>")
		}
	}
	return s
}

func RedactSecrets(secrets []string) []string {
	for i, s := range secrets {
		secrets[i] = RedactSecret(s)
	}
	return secrets
}

//
//// LogADCInfo inspects the file pointed at by GOOGLE_APPLICATION_CREDENTIALS
//// and logs non-sensitive metadata about it. It never logs private keys,
//// refresh tokens, client secrets, or credential_source headers (which
//// carry bearer tokens such as ACTIONS_ID_TOKEN_REQUEST_TOKEN on
//// GitHub-hosted runners).
//func logADCInfo() {
//	path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
//	if path == "" {
//		slog.Info("ADC: GOOGLE_APPLICATION_CREDENTIALS is not set")
//		return
//	}
//	slog.Info("ADC: GOOGLE_APPLICATION_CREDENTIALS is set", "path", path)
//
//	data, err := os.ReadFile(path)
//	if err != nil {
//		slog.Error("ADC: failed to read credentials file", "path", path, "error", err)
//		return
//	}
//
//	var creds map[string]any
//	if err := json.Unmarshal(data, &creds); err != nil {
//		slog.Error("ADC: failed to parse credentials file as JSON", "path", path, "error", err)
//		return
//	}
//
//	credType, _ := creds["type"].(string)
//	slog.Info("ADC: file summary",
//		"type", credType,
//		"size_bytes", len(data),
//		"top_level_keys", sortedKeys(creds),
//	)
//
//	switch credType {
//	case "service_account":
//		slog.Info("ADC: service_account fields",
//			"project_id", creds["project_id"],
//			"client_email", creds["client_email"],
//			"client_id", creds["client_id"],
//			"token_uri", creds["token_uri"],
//			"auth_uri", creds["auth_uri"],
//			"universe_domain", creds["universe_domain"],
//			"has_private_key", creds["private_key"] != nil,
//			"has_private_key_id", creds["private_key_id"] != nil,
//		)
//
//	case "authorized_user":
//		slog.Info("ADC: authorized_user fields",
//			"client_id", creds["client_id"],
//			"quota_project_id", creds["quota_project_id"],
//			"has_refresh_token", creds["refresh_token"] != nil,
//			"has_client_secret", creds["client_secret"] != nil,
//		)
//
//	case "external_account":
//		slog.Info("ADC: external_account fields",
//			"audience", creds["audience"],
//			"subject_token_type", creds["subject_token_type"],
//			"token_url", creds["token_url"],
//			"service_account_impersonation_url", creds["service_account_impersonation_url"],
//			"universe_domain", creds["universe_domain"],
//		)
//		if cs, ok := creds["credential_source"].(map[string]any); ok {
//			redacted := make(map[string]any, len(cs))
//			for k, v := range cs {
//				if k == "headers" {
//					// Headers commonly carry bearer tokens (e.g. GitHub Actions
//					// ACTIONS_ID_TOKEN_REQUEST_TOKEN). Log only the header names.
//					if h, ok := v.(map[string]any); ok {
//						redacted[k] = map[string]any{"header_names": sortedKeys(h)}
//					} else {
//						redacted[k] = "<REDACTED>"
//					}
//					continue
//				}
//				redacted[k] = v
//			}
//			slog.Info("ADC: external_account credential_source", "source", redacted)
//		}
//
//	case "impersonated_service_account":
//		slog.Info("ADC: impersonated_service_account fields",
//			"service_account_impersonation_url", creds["service_account_impersonation_url"],
//			"delegates", creds["delegates"],
//			"has_source_credentials", creds["source_credentials"] != nil,
//		)
//
//	default:
//		slog.Info("ADC: unknown credential type; logging key names only",
//			"keys", sortedKeys(creds),
//		)
//	}
//}
//
//func sortedKeys(m map[string]any) []string {
//	keys := make([]string, 0, len(m))
//	for k := range m {
//		keys = append(keys, k)
//	}
//	sort.Strings(keys)
//	return keys
//}
