package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	compose "github.com/compose-spec/compose-go/v2/types"
	"github.com/defang-io/defang/src/pkg/cli/client"
	"github.com/defang-io/defang/src/pkg/http"
	"github.com/defang-io/defang/src/pkg/types"
	v1 "github.com/defang-io/defang/src/protos/io/defang/v1"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	MiB                 = 1024 * 1024
	sourceDateEpoch     = 315532800 // 1980-01-01, same as nix-shell
	defaultDockerIgnore = `# Default .dockerignore file for Defang
**/.DS_Store
**/.direnv
**/.envrc
**/.git
**/.github
**/.idea
**/.next
**/.vscode
**/__pycache__
**/compose.yaml
**/compose.yml
**/defang.exe
**/docker-compose.yml
**/docker-compose.yaml
**/node_modules
**/Thumbs.db
# Ignore our own binary, but only in the root to avoid ignoring subfolders
defang`
)

var (
	nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]+`)
)

type ComposeError struct {
	error
}

func (e ComposeError) Unwrap() error {
	return e.error
}

func NormalizeServiceName(s string) string {
	return nonAlphanumeric.ReplaceAllLiteralString(strings.ToLower(s), "-")
}

func resolveEnv(k string) *string {
	// TODO: per spec, if the value is nil, then the value is taken from an interactive prompt
	v, ok := os.LookupEnv(k)
	if !ok {
		logrus.Warnf("environment variable not found: %q", k)
		HadWarnings = true
		// If the value could not be resolved, it should be removed
		return nil
	}
	return &v
}

func convertPlatform(platform string) v1.Platform {
	switch platform {
	default:
		logrus.Warnf("Unsupported platform: %q (assuming linux)", platform)
		HadWarnings = true
		fallthrough
	case "", "linux":
		return v1.Platform_LINUX_ANY
	case "linux/amd64":
		return v1.Platform_LINUX_AMD64
	case "linux/arm64", "linux/arm64/v8", "linux/arm64/v7", "linux/arm64/v6":
		return v1.Platform_LINUX_ARM64
	}
}

func LoadCompose(filePath string, tenantID types.TenantID) (*compose.Project, error) {
	return loadCompose(filePath, string(tenantID), false) // use tenantID as fallback for project name
}

func LoadComposeWithProjectName(filePath string, projectName string) (*compose.Project, error) {
	return loadCompose(filePath, projectName, true)
}

func loadCompose(filePath string, projectName string, overrideProjectName bool) (*compose.Project, error) {
	// The default path for a Compose file is compose.yaml (preferred) or compose.yml that is placed in the working directory.
	// Compose also supports docker-compose.yaml and docker-compose.yml for backwards compatibility.
	if files, _ := filepath.Glob(filePath); len(files) > 1 {
		return nil, fmt.Errorf("multiple Compose files found: %q; use -f to specify which one to use", files)
	} else if len(files) == 1 {
		filePath = files[0]
	}
	// TODO: Docker compose searches parent folders for compose files #117
	Debug(" - Loading compose file", filePath)

	// Compose-go uses the logrus logger, so we need to configure it to be more like our own logger
	logrus.SetFormatter(&logrus.TextFormatter{DisableTimestamp: true, DisableColors: !doColor(stderr), DisableLevelTruncation: true})

	loadCfg := compose.ConfigDetails{
		WorkingDir:  filepath.Dir(filePath),
		ConfigFiles: []compose.ConfigFile{{Filename: filePath}},
		Environment: map[string]string{}, // TODO: support environment variables?
	}

	loadOpts := []func(*loader.Options){
		loader.WithDiscardEnvFiles,
		func(o *loader.Options) {
			o.SkipConsistencyCheck = true // TODO: check fails if secrets are used but top-level 'secrets:' is missing
			o.SetProjectName(strings.ToLower(projectName), overrideProjectName)
		},
	}

	project, err := loader.Load(loadCfg, loadOpts...)
	if err != nil {
		return nil, err
	}

	if DoDebug {
		b, _ := yaml.Marshal(project)
		fmt.Println(string(b))
	}
	return project, nil
}

func getRemoteBuildContext(ctx context.Context, client client.Client, name string, build *compose.BuildConfig, force bool) (string, error) {
	root, err := filepath.Abs(build.Context)
	if err != nil {
		return "", fmt.Errorf("invalid build context: %w", err)
	}

	Info(" * Compressing build context for", name, "at", root)
	buffer, err := createTarball(ctx, build.Context, build.Dockerfile)
	if err != nil {
		return "", err
	}

	var digest string
	if !force {
		// Calculate the digest of the tarball and pass it to the fabric controller (to avoid building the same image twice)
		sha := sha256.Sum256(buffer.Bytes())
		digest = "sha256-" + base64.StdEncoding.EncodeToString(sha[:]) // same as Nix
		Debug(" - Digest:", digest)
	}

	if DoDryRun {
		return root, nil
	}

	Info(" * Uploading build context for", name)
	return uploadTarball(ctx, client, buffer, digest)
}

func convertPort(port compose.ServicePortConfig) (*v1.Port, error) {
	if port.Target < 1 || port.Target > 32767 {
		return nil, fmt.Errorf("port target must be an integer between 1 and 32767: %v", port.Target)
	}
	if port.HostIP != "" {
		return nil, errors.New("port host_ip is not supported")
	}
	if port.Published != "" && port.Published != strconv.FormatUint(uint64(port.Target), 10) {
		return nil, fmt.Errorf("port published must be empty or equal to target: %v", port.Published)
	}

	pbPort := &v1.Port{
		// Mode      string `yaml:",omitempty" json:"mode,omitempty"`
		// HostIP    string `mapstructure:"host_ip" yaml:"host_ip,omitempty" json:"host_ip,omitempty"`
		// Published string `yaml:",omitempty" json:"published,omitempty"`
		// Protocol  string `yaml:",omitempty" json:"protocol,omitempty"`
		Target: port.Target,
	}

	switch port.Protocol {
	case "":
		pbPort.Protocol = v1.Protocol_ANY // defaults to HTTP in CD
	case "tcp":
		pbPort.Protocol = v1.Protocol_TCP
	case "udp":
		pbPort.Protocol = v1.Protocol_UDP
	case "http": // TODO: not per spec
		pbPort.Protocol = v1.Protocol_HTTP
	case "http2": // TODO: not per spec
		pbPort.Protocol = v1.Protocol_HTTP2
	case "grpc": // TODO: not per spec
		pbPort.Protocol = v1.Protocol_GRPC
	default:
		return nil, fmt.Errorf("port protocol not one of [tcp udp http http2 grpc]: %v", port.Protocol)
	}

	logrus := logrus.WithField("target", port.Target)

	switch port.Mode {
	case "":
		logrus.Warn("No port mode was specified; assuming 'host' (add 'mode' to silence)")
		HadWarnings = true
		fallthrough
	case "host":
		pbPort.Mode = v1.Mode_HOST
	case "ingress":
		// This code is unnecessarily complex because compose-go silently converts short syntax to ingress+tcp
		if port.Published != "" {
			logrus.Warn("Published ports are not supported in ingress mode; assuming 'host' (add 'mode' to silence)")
			HadWarnings = true
			break
		}
		pbPort.Mode = v1.Mode_INGRESS
		if pbPort.Protocol == v1.Protocol_TCP || pbPort.Protocol == v1.Protocol_UDP {
			logrus.Warn("TCP ingress is not supported; assuming HTTP")
			HadWarnings = true
			pbPort.Protocol = v1.Protocol_HTTP
		}
	default:
		return nil, fmt.Errorf("port mode not one of [host ingress]: %v", port.Mode)
	}
	return pbPort, nil
}

func convertPorts(ports []compose.ServicePortConfig) ([]*v1.Port, error) {
	var pbports []*v1.Port
	for _, port := range ports {
		pbPort, err := convertPort(port)
		if err != nil {
			return nil, err
		}
		pbports = append(pbports, pbPort)
	}
	return pbports, nil
}

func uploadTarball(ctx context.Context, client client.Client, body io.Reader, digest string) (string, error) {
	// Upload the tarball to the fabric controller storage;; TODO: use a streaming API
	ureq := &v1.UploadURLRequest{Digest: digest}
	res, err := client.CreateUploadURL(ctx, ureq)
	if err != nil {
		return "", err
	}

	// Do an HTTP PUT to the generated URL
	resp, err := http.Put(ctx, res.Url, "application/gzip", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP PUT failed with status code %v", resp.Status)
	}

	return http.RemoveQueryParam(res.Url), nil
}

type contextAwareWriter struct {
	ctx context.Context
	io.WriteCloser
}

func (cw contextAwareWriter) Write(p []byte) (n int, err error) {
	select {
	case <-cw.ctx.Done(): // Detect context cancelation
		return 0, cw.ctx.Err()
	default:
		return cw.WriteCloser.Write(p)
	}
}

func tryReadIgnoreFile(cwd, ignorefile string) io.ReadCloser {
	path := filepath.Join(cwd, ignorefile)
	reader, err := os.Open(path)
	if err != nil {
		return nil
	}
	Debug(" - Reading .dockerignore file from", ignorefile)
	return reader
}

func createTarball(ctx context.Context, root, dockerfile string) (*bytes.Buffer, error) {
	foundDockerfile := false
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	} else {
		dockerfile = filepath.Clean(dockerfile)
	}

	// A Dockerfile-specific ignore-file takes precedence over the .dockerignore file at the root of the build context if both exist.
	dockerignore := dockerfile + ".dockerignore"
	reader := tryReadIgnoreFile(root, dockerignore)
	if reader == nil {
		dockerignore = ".dockerignore"
		reader = tryReadIgnoreFile(root, dockerignore)
		if reader == nil {
			Debug(" - No .dockerignore file found; using defaults")
			reader = io.NopCloser(strings.NewReader(defaultDockerIgnore))
		}
	}
	patterns, err := ignorefile.ReadAll(reader) // handles comments and empty lines
	reader.Close()
	if err != nil {
		return nil, err
	}
	pm, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, err
	}

	// TODO: use io.Pipe and do proper streaming (instead of buffering everything in memory)
	fileCount := 0
	var buf bytes.Buffer
	gzipWriter := &contextAwareWriter{ctx, gzip.NewWriter(&buf)}
	tarWriter := tar.NewWriter(gzipWriter)

	err = filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Don't include the root directory itself in the tarball
		if path == root {
			return nil
		}

		// Make sure the path is relative to the root
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		baseName := filepath.ToSlash(relPath)

		// we need the Dockerfile, even if it's in the .dockerignore file
		if !foundDockerfile && relPath == dockerfile {
			foundDockerfile = true
		} else if relPath == dockerignore {
			// we need the .dockerignore file too: it might ignore itself and/or the Dockerfile
		} else {
			// Ignore files using the dockerignore patternmatcher
			ignore, err := pm.MatchesOrParentMatches(baseName)
			if err != nil {
				return err
			}
			if ignore {
				Debug(" - Ignoring", relPath)
				if de.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		Debug(" - Adding", baseName)

		info, err := de.Info()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}

		// Make reproducible; WalkDir walks files in lexical order.
		header.ModTime = time.Unix(sourceDateEpoch, 0)
		header.Gid = 0
		header.Uid = 0
		header.Name = baseName
		err = tarWriter.WriteHeader(header)
		if err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		fileCount++
		if fileCount == 11 {
			Warn(" ! The build context contains more than 10 files; press Ctrl+C if this is unexpected.")
		}

		_, err = io.Copy(tarWriter, file)
		if buf.Len() > 10*MiB {
			return fmt.Errorf("build context is too large; this beta version is limited to 10MiB")
		}
		return err
	})

	if err != nil {
		return nil, err
	}

	// Close the tar and gzip writers before returning the buffer
	if err = tarWriter.Close(); err != nil {
		return nil, err
	}

	if err = gzipWriter.Close(); err != nil {
		return nil, err
	}

	if !foundDockerfile {
		return nil, fmt.Errorf("the specified dockerfile could not be read: %q", dockerfile)
	}

	return &buf, nil
}
