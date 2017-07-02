package images

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/containers/image/copy"
	"github.com/containers/image/signature"
	"github.com/containers/image/transports/alltransports"
	"github.com/containers/image/types"
	"github.com/mgoltzsche/runc-compose/log"
	spec "github.com/opencontainers/image-spec/specs-go/v1"
	//"github.com/opencontainers/image-tools/image"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type PullPolicy string

const (
	PULL_NEVER  PullPolicy = "never"
	PULL_NEW    PullPolicy = "new"
	PULL_UPDATE PullPolicy = "update"
)

type ImageMetadata struct {
	Name             string                `json:"name"`
	Directory        string                `json:"directory"`
	Exec             []string              `json:"exec"`
	WorkingDirectory string                `json:"workingDirectory"`
	MountPoints      map[string]string     `json:"mountPoints"`
	Ports            map[string]*ImagePort `json:"ports"`
	Environment      map[string]string     `json:"environment"`
}

type ImagePort struct {
	Protocol string `json:"protocol"`
	Port     uint16 `json:"port"`
}

type Images struct {
	images         map[string]*ImageMetadata
	imageDirectory string
	trustPolicy    *signature.PolicyContext
	pullPolicy     PullPolicy
	debug          log.Logger
}

var toIdRegexp = regexp.MustCompile("[^a-z0-9]+")

func NewImages(imageDirectory string, pullPolicy PullPolicy, debug log.Logger) (*Images, error) {
	trustPolicy, err := createTrustPolicyContext()
	if err != nil {
		return nil, fmt.Errorf("Error loading trust policy: %v", err)
	}
	return &Images{map[string]*ImageMetadata{}, imageDirectory, trustPolicy, pullPolicy, debug}, nil
}

func (self *Images) Image(name string) (*ImageMetadata, error) {
	return self.fetchImage(name, self.pullPolicy)
}

func (self *Images) fetchImage(name string, pullPolicy PullPolicy) (r *ImageMetadata, err error) {
	// TODO: use pull policy
	r = self.images[name]
	if r != nil {
		return
	}
	imgDir := self.toImageDirectory(name)
	r = &ImageMetadata{name, "", []string{}, "", map[string]string{}, map[string]*ImagePort{}, map[string]string{}}
	// Try to load image from local store
	err = readImageConfig(imgDir, r)
	if err == nil {
		self.images[name] = r
		return
	} else if pullPolicy == PULL_NEVER {
		return nil, fmt.Errorf("Cannot read local image: %v", err)
	}
	// Import image
	self.debug.Printf("Fetching image %q...", name)
	err = os.MkdirAll(imgDir, 0770)
	if err != nil {
		return nil, fmt.Errorf("Cannot create image directory: %v", err)
	}
	r.Directory = imgDir
	err = self.copyImage(name, "oci:"+imgDir)
	if err != nil {
		return nil, fmt.Errorf("Cannot fetch image: %v", err)
	}
	err = readImageConfig(imgDir, r)
	if err != nil {
		return nil, fmt.Errorf("Cannot read %q image config: %v", name, err)
	}
	// TODO: create container from OCI layout image
	//err = image.UnpackLayout(imgDir, "/tmp/oci-container", "")
	// err = image.CreateRuntimeBundleLayout(imgDir, "/tmp/oci-container", "", "/tmp/mycontainer")
	if err != nil {
		return nil, fmt.Errorf("Unpacking OCI layout failed: %v", err)
	}
	self.images[name] = r
	return
}

func (self *Images) toImageDirectory(imgName string) string {
	var buf bytes.Buffer
	encoder := base64.NewEncoder(base64.RawStdEncoding, &buf)
	encoder.Write([]byte(imgName))
	encoder.Close()
	return filepath.Join(self.imageDirectory, buf.String())
}

func readImageConfig(imgDir string, dest *ImageMetadata) error {
	var idx spec.Index
	err := unmarshalJSON(filepath.Join(imgDir, "index.json"), &idx)
	if err != nil {
		return fmt.Errorf("Cannot read OCI image index: %v", err)
	}
	for _, ref := range idx.Manifests {
		// TODO: select by Platform.Architecture, Platform.OS
		d := ref.Digest
		sep := strings.Index(string(d), ":")
		if sep < 1 {
			panic(fmt.Sprintf("no ':' separator in index digest %q", d))
		}
		algorithm := string(d.Algorithm())
		enc := string(d[sep+1:])
		manifestFile := filepath.Join(imgDir, "blobs", algorithm, enc)
		var manifest spec.Manifest
		if err = unmarshalJSON(manifestFile, &manifest); err != nil {
			return fmt.Errorf("Cannot read OCI image manifest: %v", err)
		}
		d = manifest.Config.Digest
		sep = strings.Index(string(d), ":")
		if sep < 1 {
			panic(fmt.Sprintf("no ':' separator in manifest digest %q", d))
		}
		algorithm = string(d.Algorithm())
		enc = string(d[sep+1:])
		configFile := filepath.Join(imgDir, "blobs", algorithm, enc)
		var config spec.Image
		if err = unmarshalJSON(configFile, &config); err != nil {
			return fmt.Errorf("Cannot read OCI image config: %v", err)
		}
		cfg := config.Config
		// TODO: split entrypoint/cmd
		if cfg.Entrypoint != nil {
			dest.Exec = append(dest.Exec, cfg.Entrypoint...)
		}
		if cfg.Cmd != nil {
			dest.Exec = append(dest.Exec, cfg.Cmd...)
		}
		// TODO: add user/workdir/ports
	}
	return nil
}

func unmarshalJSON(file string, dest interface{}) error {
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dest)
}

type OCIImageManifest struct {
	Config OCIImageManifestConfigRef `json:"config"`
}

type OCIImageManifestConfigRef struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

type OCIImageArchitectureSpecificConfig struct {
	Config OCIImageConfig `json:"config"`
}

type OCIImageConfig struct {
	User       string
	WorkingDir string
	Entrypoint []string
	Cmd        []string
}

func (self *Images) copyImage(src, dest string) error {
	srcRef, err := alltransports.ParseImageName(src)
	if err != nil {
		return fmt.Errorf("Invalid image source %s: %v", src, err)
	}
	destRef, err := alltransports.ParseImageName(dest)
	if err != nil {
		return fmt.Errorf("Invalid image destination %s: %v", dest, err)
	}
	systemCtx := &types.SystemContext{
		RegistriesDirPath:           "",
		DockerCertPath:              "",
		DockerInsecureSkipTLSVerify: true,
		OSTreeTmpDirPath:            "ostree-tmp-dir",
		// TODO: add docker auth
		//DockerAuthConfig: dockerAuth,
	}
	return copy.Image(self.trustPolicy, destRef, srcRef, &copy.Options{
		RemoveSignatures: false,
		SignBy:           "",
		ReportWriter:     os.Stdout,
		SourceCtx:        systemCtx,
		DestinationCtx:   systemCtx,
	})
}

func createTrustPolicyContext() (*signature.PolicyContext, error) {
	policyFile := ""
	var policy *signature.Policy // This could be cached across calls, if we had an application context.
	var err error
	//if insecurePolicy {
	//	policy = &signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}}
	if policyFile == "" {
		policy, err = signature.DefaultPolicy(nil)
	} else {
		policy, err = signature.NewPolicyFromFile(policyFile)
	}
	if err != nil {
		return nil, err
	}
	return signature.NewPolicyContext(policy)
}

func (self *Images) BuildImage(uri, dockerFile, contextPath string) (*ImageMetadata, error) {
	name := uri
	if len(uri) > 14 && uri[0:14] == "docker-daemon:" {
		name = uri[14:]
	}
	img, err := self.fetchImage(uri, PULL_NEVER)
	if err == nil {
		return img, nil
	}
	imgFile := filepath.FromSlash(dockerFile)
	dockerFileDir := filepath.Dir(imgFile)
	if contextPath == "" {
		contextPath = dockerFileDir
	}
	self.debug.Printf("Building docker image from %q...", imgFile)
	c := exec.Command("docker", "build", "-t", name, "--rm", dockerFileDir)
	c.Dir = contextPath
	c.Stdout = os.Stdout // TODO: write to log
	c.Stderr = os.Stderr
	if err = c.Run(); err != nil {
		return nil, err
	}
	img, err = self.fetchImage(uri, PULL_UPDATE)
	if err != nil {
		return nil, err
	}
	self.images[name] = img
	return img, nil
}

func toId(v string) string {
	return strings.Trim(toIdRegexp.ReplaceAllLiteralString(strings.ToLower(v), "-"), "-")
}

func removeFile(file string) {
	e := os.Remove(file)
	if e != nil {
		os.Stderr.WriteString(fmt.Sprintf("image loader: %s\n", e))
	}
}
