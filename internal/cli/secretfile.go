package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// createdByLabel marks the Secrets that --secret-file wrote (spec D2 5.2).
const createdByLabel, createdByValue = "ambitproject.dev/created-by", "ambit-cli"

// secretFile is one --secret-file <secret>/<key>=<path>. value is read once, before anything else is done
// with it, and registered with the redactor first.
type secretFile struct {
	Secret, Key, Path string
	value             []byte
}

var (
	secretNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	secretKeyRE  = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
)

// secretNames are the Secrets the values name by reference (and so the only ones --secret-file may write), with
// the setting that names each. The chart reads all of them by name only.
func secretNames(vals map[string]any) map[string]string {
	names := map[string]string{}
	add := func(name, from string) {
		if name != "" {
			names[name] = from
		}
	}
	if envs, ok := lookup(vals, "dex.envVars").([]any); ok {
		for _, e := range envs {
			m, _ := e.(map[string]any)
			if s, ok := lookup(m, "valueFrom.secretKeyRef.name").(string); ok {
				add(s, "dex.envVars")
			}
		}
	}
	for _, path := range []string{"gateway.tls.certificateSecret", "evidence.clickhouse.external.passwordSecret", "limits.valkey.external.passwordSecret"} {
		s, _ := lookup(vals, path).(string)
		add(s, path)
	}
	if on, _ := lookup(vals, "gitops.enabled").(bool); on {
		s, _ := lookup(vals, "gitops.github.privateKeySecret").(string)
		if s == "" {
			s = "ambit-github-app" // the chart's default
		}
		add(s, "gitops.github.privateKeySecret")
	}
	return names
}

func lookup(m map[string]any, path string) any {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// readSecretFiles parses each --secret-file, refuses a Secret the values do not name (so a typo cannot create a
// stray Secret), reads the file and registers its bytes with the redactor before anything else uses them.
func readSecretFiles(specs []string, vals map[string]any, r *out.Redactor) ([]secretFile, error) {
	allowed := secretNames(vals)
	var files []secretFile
	seen := map[string]bool{}
	for _, spec := range specs {
		ref, path, ok := strings.Cut(spec, "=")
		name, key, ok2 := strings.Cut(ref, "/")
		if !ok || !ok2 || path == "" || !secretNameRE.MatchString(name) || !secretKeyRE.MatchString(key) {
			return nil, out.Usage("--secret-file "+spec, "the form is <secret>/<key>=<path>, e.g. ambit-sso/clientSecret=./client-secret.txt", "ambit install --help")
		}
		if allowed[name] == "" {
			return nil, out.Usage("--secret-file "+spec, fmt.Sprintf("the values do not name a Secret %q (names they use: %s)", name, joinKeys(allowed)),
				"use a Secret name from dex.envVars, gateway.tls.certificateSecret, evidence.clickhouse.external.passwordSecret, limits.valkey.external.passwordSecret or gitops.github.privateKeySecret")
		}
		if seen[name+"/"+key] {
			return nil, out.Usage("--secret-file "+spec, "the same Secret key is given twice", "give each <secret>/<key> once")
		}
		seen[name+"/"+key] = true
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, out.Usage("--secret-file "+spec, err.Error(), "check the path")
		}
		r.Add(string(b))
		if t := strings.TrimSpace(string(b)); t != string(b) {
			r.Add(t)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return nil, out.Usage("--secret-file "+spec, "the file is empty", "check the path")
		}
		files = append(files, secretFile{Secret: name, Key: key, Path: path, value: b})
	}
	return files, nil
}

func joinKeys(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

// writeSecretFiles creates or updates each Secret with the Kubernetes API, not through Helm, so the bytes are in
// no Helm release. A merge patch sets only the given keys and the label: the Secret's other keys stay, and the CLI
// never reads the Secret back. A Secret that does not exist is created (type kubernetes.io/tls for the gateway's
// certificate Secret). The namespace is created first when it does not exist.
func writeSecretFiles(ctx context.Context, cs kubernetes.Interface, ns string, files []secretFile, vals map[string]any) error {
	if len(files) == 0 {
		return nil
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create namespace %s: %w", ns, err)
		}
	} else if err != nil {
		return fmt.Errorf("read namespace %s: %w", ns, err)
	}
	bySecret := map[string]map[string][]byte{}
	var order []string
	for _, f := range files {
		if bySecret[f.Secret] == nil {
			bySecret[f.Secret] = map[string][]byte{}
			order = append(order, f.Secret)
		}
		bySecret[f.Secret][f.Key] = f.value
	}
	tlsName, _ := lookup(vals, "gateway.tls.certificateSecret").(string)
	for _, name := range order {
		data := bySecret[name]
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"labels": map[string]string{createdByLabel: createdByValue}},
			"data":     data, // []byte marshals as base64, as the API wants
		})
		if err != nil {
			return err
		}
		_, err = cs.CoreV1().Secrets(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: cliFieldManager})
		if apierrors.IsNotFound(err) {
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{createdByLabel: createdByValue}}, Data: data}
			if name == tlsName {
				s.Type = corev1.SecretTypeTLS
			}
			_, err = cs.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{FieldManager: cliFieldManager})
		}
		if err != nil {
			// The API's error text names the Secret, never its data; the redactor covers the rest.
			return fmt.Errorf("Secret %s: %w", name, err)
		}
	}
	return nil
}

// describe is "Secret ambit-sso (keys clientSecret, from ./sso-secret.txt)" for the output; never the value.
func describeSecretFiles(files []secretFile) []string {
	var lines []string
	for _, f := range files {
		lines = append(lines, fmt.Sprintf("Secret %s key %s, from %s", f.Secret, f.Key, f.Path))
	}
	return lines
}
