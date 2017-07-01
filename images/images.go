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
	imgJsonFile := imgDir + ".json"
	r = &ImageMetadata{name, "", []string{}, "", map[string]string{}, map[string]*ImagePort{}, map[string]string{}}
	// Try to load image from local store
	err = unmarshalJSON(imgJsonFile, r)
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
	imgType := strings.SplitN(name, ":", 2)[0]
	if imgType == "docker" || imgType == "docker-daemon" {
		tmpDir, err := ioutil.TempDir("", "image-")
		if err != nil {
			return nil, fmt.Errorf("Cannot create image temp directory: %s", err)
		}
		defer os.RemoveAll(tmpDir)
		err = self.copyImage(name, "dir:"+tmpDir)
		if err != nil {
			return nil, fmt.Errorf("Cannot fetch image: %v", err)
		}
		err = convertDockerImageConfig(tmpDir, r)
		if err != nil {
			return nil, fmt.Errorf("Cannot read docker image config: %v", err)
		}
		err = self.copyImage("dir:"+tmpDir, "oci:"+imgDir)
		if err != nil {
			return nil, fmt.Errorf("Cannot copy temporary docker image into store: %v", err)
		}
	} else {
		err = self.copyImage(name, "oci:"+imgDir)
		if err != nil {
			return nil, fmt.Errorf("Cannot fetch image: %v", err)
		}
	}
	json, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("Cannot marshal image config: %v", err)
	}
	err = ioutil.WriteFile(imgJsonFile, json, 0660)
	if err != nil {
		return nil, fmt.Errorf("Cannot write image config: %v", err)
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

func convertDockerImageConfig(dir string, dest *ImageMetadata) error {
	var manifest dockerImageManifest
	err := unmarshalJSON(filepath.Join(dir, "manifest.json"), &manifest)
	if err != nil {
		return fmt.Errorf("Cannot read docker image manifest: %v", err)
	}
	if manifest.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
		return fmt.Errorf("Unsupported docker image manifest config media type %q", manifest.Config.MediaType)
	}
	digestParts := strings.SplitN(manifest.Config.Digest, ":", 2)
	if len(digestParts) != 2 {
		return fmt.Errorf("Unsupported manifest config digest %q", manifest.Config.Digest)
	}
	cfgFile := filepath.Join(dir, digestParts[1]+".tar")
	var conf dockerImageMetadata
	err = unmarshalJSON(cfgFile, &conf)
	cfg := conf.Config
	// TODO: split entrypoint/cmd
	if cfg.Entrypoint != nil {
		dest.Exec = append(dest.Exec, cfg.Entrypoint...)
	}
	if cfg.Cmd != nil {
		dest.Exec = append(dest.Exec, cfg.Cmd...)
	}
	// TODO: add user/workdir/ports
	return nil
}

func unmarshalJSON(file string, dest interface{}) error {
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dest)
}

type dockerImageManifest struct {
	Config dockerImageManifestConfigRef `json:"config"`
}

type dockerImageManifestConfigRef struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

type dockerImageMetadata struct {
	Config dockerImageConfig `json:"config"`
}

type dockerImageConfig struct {
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
