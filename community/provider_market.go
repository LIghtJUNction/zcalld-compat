package community

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This is the same provider-plugin registry used by upstream Magpie.
const magpieProviderMarket = "https://raw.githubusercontent.com/magpie-community/plugins/main/registry.json"
const magpieDist = "https://raw.githubusercontent.com/magpie-community/plugins/dist"
const providerMarketLimit = 16 << 20

var providerPackage = regexp.MustCompile(`^(@[a-z0-9][a-z0-9_.-]*/)?[a-z0-9][a-z0-9_.-]*$`)

type providerListing struct {
	Package   string   `json:"package"`
	Name      string   `json:"name"`
	Providers []string `json:"providers"`
}

func publicBytes(address string, limit int64) ([]byte, error) {
	client := http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get(address)
	if err != nil {
		return nil, errors.New("could not read the public plugin registry")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("the public plugin registry did not return an entry")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("public plugin response is unreadable or too large")
	}
	return body, nil
}

func publicJSON(address string, target any) error {
	body, err := publicBytes(address, providerMarketLimit)
	if err != nil {
		return err
	}
	if json.Unmarshal(body, target) != nil {
		return errors.New("invalid public plugin registry response")
	}
	return nil
}

type distArtifact struct {
	File      string `json:"file"`
	Integrity string `json:"integrity"`
}

var distFile = regexp.MustCompile(`^tgz/[A-Za-z0-9._-]+\.tgz$`)

func magpieRelease(pkg, version string) (string, distArtifact, error) {
	var index struct {
		Packages map[string]struct {
			Latest   string                  `json:"latest"`
			Versions map[string]distArtifact `json:"versions"`
		} `json:"packages"`
	}
	if err := publicJSON(magpieDist+"/index.json", &index); err != nil {
		return "", distArtifact{}, err
	}
	release := index.Packages[pkg]
	if version == "" {
		version = release.Latest
	}
	artifact := release.Versions[version]
	if !npmVersion.MatchString(pkg+"@"+version) || !distFile.MatchString(artifact.File) || !strings.HasPrefix(artifact.Integrity, "sha512-") {
		return "", distArtifact{}, errors.New("version is missing from the Magpie package distribution")
	}
	digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(artifact.Integrity, "sha512-"))
	if err != nil || len(digest) != sha512.Size {
		return "", distArtifact{}, errors.New("invalid Magpie package integrity")
	}
	return version, artifact, nil
}

func installMagpiePackage(stage, source string) (string, error) {
	spec, integrity, pinned := strings.Cut(strings.TrimPrefix(source, "magpie:"), "#")
	pkg := npmName(spec)
	version := strings.TrimPrefix(spec, pkg+"@")
	if !pinned || !strings.HasPrefix(pkg, "@magpie-community/") || !npmVersion.MatchString(spec) {
		return "", errors.New("Magpie installs require a pinned community distribution package")
	}
	_, artifact, err := magpieRelease(pkg, version)
	if err != nil {
		return "", err
	}
	if integrity != artifact.Integrity {
		return "", errors.New("Magpie package integrity changed after selection")
	}
	body, err := publicBytes(magpieDist+"/"+artifact.File, 32<<20)
	if err != nil {
		return "", err
	}
	digest := sha512.Sum512(body)
	if "sha512-"+base64.StdEncoding.EncodeToString(digest[:]) != integrity {
		return "", errors.New("Magpie package checksum mismatch")
	}
	tarball := filepath.Join(stage, "plugin.tgz")
	if err = Atomic(tarball, body, 0600); err != nil {
		return "", err
	}
	if err = Atomic(filepath.Join(stage, "package.json"), []byte("{\"private\":true}"), 0600); err != nil {
		return "", err
	}
	if err = run(stage, "bun", "add", "--ignore-scripts", "--no-progress", "--exact", "./plugin.tgz"); err != nil {
		return "", err
	}
	target := filepath.Join(stage, "node_modules", filepath.FromSlash(pkg))
	manifest, err := readLimit(filepath.Join(target, "package.json"), 1<<20)
	var identity struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err != nil || json.Unmarshal(manifest, &identity) != nil || identity.Name != pkg || identity.Version != version {
		return "", errors.New("installed Magpie package does not match the approved identity")
	}
	return target, nil
}

// ProviderMarket lists provider entries from Magpie. A selected package is
// resolved to an exact npm version for the existing install confirmation.
func ProviderMarket(pkg string) (map[string]any, error) {
	var registry struct {
		Plugins []providerListing `json:"plugins"`
	}
	if err := publicJSON(magpieProviderMarket, &registry); err != nil {
		return nil, err
	}
	listings := make([]providerListing, 0, len(registry.Plugins))
	for _, item := range registry.Plugins {
		if len(item.Package) > 214 || !providerPackage.MatchString(item.Package) || len(item.Providers) == 0 {
			continue
		}
		listings = append(listings, item)
		if pkg != "" && item.Package == pkg {
			var metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			address := "https://registry.npmjs.org/" + url.PathEscape(pkg) + "/latest"
			spec := ""
			resolved := ""
			npmError := publicJSON(address, &metadata)
			if npmError == nil && metadata.Name == pkg {
				spec = pkg + "@" + metadata.Version
				if npmVersion.MatchString(spec) {
					resolved = "npm:" + spec
				}
			}
			if resolved == "" && strings.HasPrefix(pkg, "@magpie-community/") {
				version, artifact, err := magpieRelease(pkg, "")
				if err != nil {
					return nil, err
				}
				resolved = "magpie:" + pkg + "@" + version + "#" + artifact.Integrity
			}
			if resolved == "" {
				return nil, errors.New("the plugin does not have an exact published version")
			}
			name := strings.TrimPrefix(pkg, "@")
			if _, basename, scoped := strings.Cut(name, "/"); scoped {
				name = basename
			}
			return map[string]any{"plugin": item, "source": resolved, "name": name}, nil
		}
	}
	if pkg != "" {
		return nil, errors.New("plugin is not in the Magpie provider registry")
	}
	return map[string]any{"plugins": listings}, nil
}
