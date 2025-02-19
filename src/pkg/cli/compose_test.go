package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/defang-io/defang/src/pkg/cli/client"
	v1 "github.com/defang-io/defang/src/protos/io/defang/v1"
	"github.com/sirupsen/logrus"
)

func TestNormalizeServiceName(t *testing.T) {
	testCases := []struct {
		name     string
		expected string
	}{
		{name: "normal", expected: "normal"},
		{name: "camelCase", expected: "camelcase"},
		{name: "PascalCase", expected: "pascalcase"},
		{name: "hyphen-ok", expected: "hyphen-ok"},
		{name: "snake_case", expected: "snake-case"},
		{name: "$ymb0ls", expected: "-ymb0ls"},
		{name: "consecutive--hyphens", expected: "consecutive-hyphens"},
		{name: "hyphen-$ymbol", expected: "hyphen-ymbol"},
		{name: "_blah", expected: "-blah"},
	}
	for _, tC := range testCases {
		t.Run(tC.name, func(t *testing.T) {
			actual := NormalizeServiceName(tC.name)
			if actual != tC.expected {
				t.Errorf("NormalizeServiceName() failed: expected %v, got %v", tC.expected, actual)
			}
		})
	}
}

func TestLoadCompose(t *testing.T) {
	DoVerbose = true
	DoDebug = true

	t.Run("no project name defaults to tenantID", func(t *testing.T) {
		p, err := LoadComposeWithProjectName("../../tests/noprojname/compose.yaml", "tenant-id")
		if err != nil {
			t.Fatalf("LoadCompose() failed: %v", err)
		}
		if p.Name != "tenant-id" {
			t.Errorf("LoadCompose() failed: expected project name tenant-id, got %q", p.Name)
		}
	})

	t.Run("use project name", func(t *testing.T) {
		p, err := LoadComposeWithProjectName("../../tests/testproj/compose.yaml", "tests")
		if err != nil {
			t.Fatalf("LoadCompose() failed: %v", err)
		}
		if p.Name != "tests" {
			t.Errorf("LoadCompose() failed: expected project name, got %q", p.Name)
		}
	})

	t.Run("fancy project name", func(t *testing.T) {
		p, err := LoadComposeWithProjectName("../../tests/noprojname/compose.yaml", "Valid-Username")
		if err != nil {
			t.Fatalf("LoadCompose() failed: %v", err)
		}
		if p.Name != "valid-username" {
			t.Errorf("LoadCompose() failed: expected project name, got %q", p.Name)
		}
	})

	t.Run("no project name defaults to tenantID", func(t *testing.T) {
		p, err := LoadCompose("../../tests/noprojname/compose.yaml", "tenant-id")
		if err != nil {
			t.Fatalf("LoadCompose() failed: %v", err)
		}
		if p.Name != "tenant-id" {
			t.Errorf("LoadCompose() failed: expected project name tenant-id, got %q", p.Name)
		}
	})

	t.Run("use project name should not be overriden by tenantID", func(t *testing.T) {
		p, err := LoadCompose("../../tests/testproj/compose.yaml", "tenant-id")
		if err != nil {
			t.Fatalf("LoadCompose() failed: %v", err)
		}
		if p.Name != "tests" {
			t.Errorf("LoadCompose() failed: expected project name tests, got %q", p.Name)
		}
	})

	t.Run("no project name defaults to tenantID", func(t *testing.T) {
		p, err := LoadCompose("../../tests/noprojname/compose.yaml", "tenanT-id")
		if err != nil {
			t.Fatalf("LoadCompose() failed: %v", err)
		}
		if p.Name != "tenant-id" {
			t.Errorf("LoadCompose() failed: expected project name tenant-id, got %q", p.Name)
		}
	})

}

func TestConvertPort(t *testing.T) {
	tests := []struct {
		name     string
		input    types.ServicePortConfig
		expected *v1.Port
		wantErr  string
	}{
		{
			name:    "No target port xfail",
			input:   types.ServicePortConfig{},
			wantErr: "port target must be an integer between 1 and 32767",
		},
		{
			name:     "Undefined mode and protocol, target only",
			input:    types.ServicePortConfig{Target: 1234},
			expected: &v1.Port{Target: 1234, Mode: v1.Mode_HOST},
		},
		{
			name:    "Published range xfail",
			input:   types.ServicePortConfig{Target: 1234, Published: "1111-2222"},
			wantErr: "port published must be empty or equal to target: 1111-2222",
		},
		{
			name:     "Implied ingress mode, defined protocol, published equals target",
			input:    types.ServicePortConfig{Mode: "ingress", Protocol: "tcp", Published: "1234", Target: 1234},
			expected: &v1.Port{Target: 1234, Mode: v1.Mode_HOST, Protocol: v1.Protocol_TCP},
		},
		{
			name:     "Implied ingress mode, udp protocol, published equals target",
			input:    types.ServicePortConfig{Mode: "ingress", Protocol: "udp", Published: "1234", Target: 1234},
			expected: &v1.Port{Target: 1234, Mode: v1.Mode_HOST, Protocol: v1.Protocol_UDP},
		},
		{
			name:    "Localhost IP, unsupported mode and protocol xfail",
			input:   types.ServicePortConfig{Mode: "ingress", HostIP: "127.0.0.1", Protocol: "tcp", Published: "1234", Target: 1234},
			wantErr: "host_ip is not supported",
		},
		{
			name:     "Ingress mode without host IP, single target",
			input:    types.ServicePortConfig{Mode: "ingress", Protocol: "tcp", Target: 1234},
			expected: &v1.Port{Target: 1234, Mode: v1.Mode_INGRESS, Protocol: v1.Protocol_HTTP},
		},
		{
			name:    "Ingress mode without host IP, single target, published range xfail",
			input:   types.ServicePortConfig{Mode: "ingress", Protocol: "tcp", Target: 1234, Published: "1111-2223"},
			wantErr: "port published must be empty or equal to target: 1111-2223",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertPort(tt.input)
			if err != nil {
				if tt.wantErr == "" {
					t.Errorf("convertPort() unexpected error: %v", err)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("convertPort() error = %v, wantErr %v", err, tt.wantErr)
				}
				return
			}
			if got.String() != tt.expected.String() {
				t.Errorf("convertPort() got %v, want %v", got, tt.expected.String())
			}
		})
	}
}

func TestUploadTarball(t *testing.T) {
	const path = "/upload/x/"
	const digest = "sha256-47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("Expected PUT request, got %v", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, path) {
			t.Errorf("Expected prefix %v, got %v", path, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/gzip" {
			t.Errorf("Expected Content-Type: application/gzip, got %v", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(200)
	}))
	defer server.Close()

	t.Run("upload with digest", func(t *testing.T) {
		url, err := uploadTarball(context.TODO(), client.MockClient{UploadUrl: server.URL + path}, &bytes.Buffer{}, digest)
		if err != nil {
			t.Fatalf("uploadTarball() failed: %v", err)
		}
		const expectedPath = path + digest
		if url != server.URL+expectedPath {
			t.Errorf("Expected %v, got %v", server.URL+expectedPath, url)
		}
	})

	t.Run("force upload without digest", func(t *testing.T) {
		url, err := uploadTarball(context.TODO(), client.MockClient{UploadUrl: server.URL + path}, &bytes.Buffer{}, "")
		if err != nil {
			t.Fatalf("uploadTarball() failed: %v", err)
		}
		if url != server.URL+path {
			t.Errorf("Expected %v, got %v", server.URL+path, url)
		}
	})
}

func TestCreateTarballReader(t *testing.T) {
	t.Run("Default Dockerfile", func(t *testing.T) {
		buffer, err := createTarball(context.TODO(), "../../tests/testproj", "")
		if err != nil {
			t.Fatalf("createTarballReader() failed: %v", err)
		}

		g, err := gzip.NewReader(buffer)
		if err != nil {
			t.Fatalf("gzip.NewReader() failed: %v", err)
		}
		defer g.Close()

		expected := []string{".dockerignore", "Dockerfile", "fileName.env"}
		var actual []string
		ar := tar.NewReader(g)
		for {
			h, err := ar.Next()
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			// Ensure the paths are relative
			if h.Name[0] == '/' {
				t.Errorf("Path is not relative: %v", h.Name)
			}
			if _, err := ar.Read(make([]byte, h.Size)); err != io.EOF {
				t.Log(err)
			}
			actual = append(actual, h.Name)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("Expected files: %v, got %v", expected, actual)
		}
	})

	t.Run("Missing Dockerfile", func(t *testing.T) {
		_, err := createTarball(context.TODO(), "../../tests", "Dockerfile.missing")
		if err == nil {
			t.Fatal("createTarballReader() should have failed")
		}
	})

	t.Run("Missing Context", func(t *testing.T) {
		_, err := createTarball(context.TODO(), "asdfqwer", "")
		if err == nil {
			t.Fatal("createTarballReader() should have failed")
		}
	})
}

type MockClient struct {
	client.Client
}

func (m MockClient) Deploy(ctx context.Context, req *v1.DeployRequest) (*v1.DeployResponse, error) {
	return &v1.DeployResponse{}, nil
}

func TestProjectValidationServiceName(t *testing.T) {
	p, err := LoadCompose("../../tests/testproj/compose.yaml", "tests")
	if err != nil {
		t.Fatalf("LoadCompose() failed: %v", err)
	}

	if err := validateProject(p); err != nil {
		t.Fatalf("Project validation failed: %v", err)
	}

	svc := p.Services["dfnx"]
	longName := "aVeryLongServiceNameThatIsDefinitelyTooLongThatWillCauseAnError"
	svc.Name = longName
	p.Services[longName] = svc

	if err := validateProject(p); err == nil {
		t.Fatalf("Long project name should be an error")
	}

}

func TestProjectValidationNetworks(t *testing.T) {
	var warnings bytes.Buffer
	logrus.SetOutput(&warnings)

	p, err := LoadCompose("../../tests/testproj/compose.yaml", "tests")
	if err != nil {
		t.Fatalf("LoadCompose() failed: %v", err)
	}

	dfnx := p.Services["dfnx"]
	dfnx.Networks = map[string]*types.ServiceNetworkConfig{"invalid-network-name": nil}
	p.Services["dfnx"] = dfnx
	if err := validateProject(p); err != nil {
		t.Errorf("Invalid network name should not be an error: %v", err)
	}
	if !bytes.Contains(warnings.Bytes(), []byte("network invalid-network-name used by service dfnx is not defined")) {
		t.Errorf("Invalid network name should trigger a warning")
	}

	warnings.Reset()
	dfnx.Networks = map[string]*types.ServiceNetworkConfig{"public": nil}
	p.Services["dfnx"] = dfnx
	if err := validateProject(p); err != nil {
		t.Errorf("public network name should not be an error: %v", err)
	}
	if !bytes.Contains(warnings.Bytes(), []byte("network public used by service dfnx is not defined")) {
		t.Errorf("missing public network in global networks section should trigger a warning")
	}

	warnings.Reset()
	p.Networks["public"] = types.NetworkConfig{}
	if err := validateProject(p); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if bytes.Contains(warnings.Bytes(), []byte("network public used by service dfnx is not defined")) {
		t.Errorf("When public network is defined globally should not trigger a warning when public network is used")
	}
}

func TestProjectValidationNoDeploy(t *testing.T) {
	p, err := LoadCompose("../../tests/testproj/compose.yaml", "tests")
	if err != nil {
		t.Fatalf("LoadCompose() failed: %v", err)
	}

	dfnx := p.Services["dfnx"]
	dfnx.Deploy = nil
	p.Services["dfnx"] = dfnx
	if err := validateProject(p); err != nil {
		t.Errorf("No deploy section should not be an error: %v", err)
	}
}
