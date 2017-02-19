package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

var image string
var keep bool
var alias string

const (
	cloudInitMetaTemplate = `#cloud-config
instance-id: {{ container.name }}
local-hostname: {{ container.name }}
{{ config_get("user.meta-data", "") }}`

	cloudInitNetworkTemplate = `{% if config_get("user.network-config", "") == "" %}version: 1
config:
    - type: physical
      name: eth0
      subnets:
          - type: {% if config_get("user.network_mode", "") == "link-local" %}manual{% else %}dhcp{% endif %}
            control: auto{% else %}{{ config_get("user.network-config", "") }}{% endif %}`

	cloudInitUserTemplate = `{{ config_get("user.user-data", properties.default) }}`

	cloudInitVendorTemplate = `{{ config_get("user.vendor-data", properties.default) }}`
)

var cloudInitTemplates = map[string]template{
	"/var/lib/cloud/seed/nocloud-net/meta-data": template{
		Template: "cloud-init-meta.tpl",
		When:     []string{"create", "copy"},
		content:  cloudInitMetaTemplate,
	},
	"/var/lib/cloud/seed/nocloud-net/network-config": template{
		Template: "cloud-init-network.tpl",
		When:     []string{"create", "copy"},
		content:  cloudInitNetworkTemplate,
	},
	"/var/lib/cloud/seed/nocloud-net/user-data": template{
		Properties: map[string]string{
			"default": "#cloud-config\n{}",
		},
		Template: "cloud-init-user.tpl",
		When:     []string{"create", "copy"},
		content:  cloudInitUserTemplate,
	},
	"/var/lib/cloud/seed/nocloud-net/vendor-data": template{
		Properties: map[string]string{
			"default": "#cloud-config\n{}",
		},
		Template: "cloud-init-vendor.tpl",
		When:     []string{"create", "copy"},
		content:  cloudInitVendorTemplate,
	},
}

type template struct {
	Properties map[string]string `yaml:"properties,omitempty"`
	Template   string            `yaml:"template"`
	When       []string          `yaml:"when,omitempty"`

	// content is the contents of the template file to create
	// in the image metadata.
	content string `yaml:"-"`
}

func Main() error {
	flag.StringVar(&image, "image", "images:centos/7", "Base CentOS image")
	flag.StringVar(&alias, "alias", "juju/centos7/amd64", "Alias for new image")
	flag.BoolVar(&keep, "keep", false, "Keep the build directory")
	flag.Parse()

	tmpdir, err := ioutil.TempDir("", "juju-lxd-centos")
	if err != nil {
		return err
	}
	if keep {
		log.Println("Build directory:", tmpdir)
	} else {
		defer os.RemoveAll(tmpdir)
	}

	// Start a build container.
	var deleted bool
	containerName := fmt.Sprintf("juju-lxd-centos-%v", time.Now().Unix())
	if err := lxc("launch", image, containerName); err != nil {
		return err
	}
	if keep {
		log.Println("Build container:", containerName)
	} else {
		defer func() {
			if deleted {
				return
			}
			err := lxc("delete", "--force", containerName)
			if err != nil {
				log.Println("Deleting build container", err)
			}
		}()
	}

	// Update the build container by running commands inside it,
	// and then publish the container as an image.
	if err := waitContainerNetwork(containerName); err != nil {
		return err
	}
	if err := updateContainer(containerName); err != nil {
		return err
	}
	if err := lxc("stop", containerName); err != nil {
		return err
	}
	if err := lxc("publish", "--alias="+alias, containerName); err != nil {
		return err
	}
	if err := lxc("delete", containerName); err != nil {
		return err
	}
	deleted = true

	// Export the image and add the cloud-init templates.
	if err := updateImageTemplates(alias, tmpdir); err != nil {
		return err
	}

	return nil
}

func waitContainerNetwork(container string) error {
	log.Println("Waiting for network connectivity")

	now := time.Now()
	interval := time.Second
	deadline := now.Add(time.Minute)
	for !now.After(deadline) {
		status, err := getContainerStatus(container)
		if err != nil {
			return err
		}
		if status.State.Status == "Running" {
			for name, network := range status.State.Networks {
				if name == "lo" || network.State != "up" || len(network.Addresses) == 0 {
					continue
				}
				for _, addr := range network.Addresses {
					if addr.Scope == "global" && addr.Family == "inet" {
						return nil
					}
				}
			}
		}
		time.Sleep(interval)
		now = now.Add(interval)
	}
	return errors.New("timed out waiting for network connectivity")
}

type containerStatus struct {
	State struct {
		Status   string `json:"status"`
		Networks map[string]struct {
			Addresses []struct {
				Family string `json:"family"`
				Scope  string `json:"scope"`
			} `json:"addresses"`
			State string `json:"state"`
		} `json:"network"`
	} `json:"state"`
}

func getContainerStatus(container string) (*containerStatus, error) {
	var buf bytes.Buffer
	cmd := exec.Command("lxc", "list", "--format=json", container)
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var statuses []containerStatus
	if err := json.Unmarshal(buf.Bytes(), &statuses); err != nil {
		return nil, err
	}
	return &statuses[0], nil
}

func updateContainer(container string) error {
	commands := []string{
		"yum install -y openssh-server redhat-lsb-core cloud-init",
		// Disable the set_hostname/update_hostname modules, or SELinux sadness ensues.
		"sed -i -E 's/.*(set|update)_hostname.*/#\\0/' /etc/cloud/cloud.cfg",
	}
	for _, command := range commands {
		if err := lxc("exec", container, "--", "/bin/sh", "-c", command); err != nil {
			return err
		}
	}
	return nil
}

func updateImageTemplates(alias, tmpdir string) error {
	if err := lxc("image", "export", alias, tmpdir); err != nil {
		return err
	}

	// Images can have one of two formats: a single tarball with
	// both rootfs and metadata in it, or separate rootfs and
	// metadata tarballs.
	//
	// We currently assume that the centos/7 image uses a single
	// tarball only.
	f, err := os.Open(tmpdir)
	if err != nil {
		return err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	if len(names) != 1 {
		return fmt.Errorf(
			"expected a single tarball, found %v (%s)",
			len(names), names,
		)
	}

	// Decompress the tarball, so we can update its contents. We do it
	// like this rather than extracting the whole tarball with "tar xf"
	// to avoid having to run as root, since the tarball contains root-
	// owned special files.
	tarballName := names[0]
	fingerprint := tarballName[:strings.IndexRune(tarballName, '.')]
	switch ext := path.Ext(tarballName); ext {
	case ".gz":
		if err := run("gunzip", filepath.Join(tmpdir, tarballName)); err != nil {
			return err
		}
		tarballName = strings.TrimSuffix(tarballName, ext)
	default:
		fmt.Println(path.Ext(tarballName))
		return fmt.Errorf("Unhandled compression type in tarball: %s", tarballName)
	}

	// Extract metadata.yaml, and update it with the cloud-init
	// template references. Also write the templates to disk in
	// the temp dir, and then update the tarball.
	var metadataBuf bytes.Buffer
	tarCmd := exec.Command("tar", "xOf", tarballName, "metadata.yaml")
	tarCmd.Stdin = os.Stdin
	tarCmd.Stdout = &metadataBuf
	tarCmd.Stderr = os.Stderr
	tarCmd.Dir = tmpdir
	if err := tarCmd.Run(); err != nil {
		return err
	}
	metadata := make(map[string]interface{})
	if err := yaml.Unmarshal(metadataBuf.Bytes(), &metadata); err != nil {
		return err
	}

	// Update the metadata with the cloud-init template references,
	// writing it and the template to disk in the temp dir, so we
	// can update the tarball.
	templates := metadata["templates"].(map[interface{}]interface{})
	for name, template := range cloudInitTemplates {
		templates[name] = template
	}
	metadataOut, err := yaml.Marshal(metadata)
	if err != nil {
		return err
	}

	log.Println("Updating metadata/templates in tarball")
	outTarballName := filepath.Join(tmpdir, "output.tar.gz")
	if err := createFinalTarball(
		outTarballName,
		filepath.Join(tmpdir, tarballName),
		metadataOut,
		gzip.DefaultCompression,
	); err != nil {
		return err
	}

	// Import the image tarball over the top of the alias, and finally
	// remove the intermediate image.
	if err := lxc("image", "import", "--alias="+alias, outTarballName); err != nil {
		return err
	}
	if err := lxc("image", "delete", fingerprint); err != nil {
		return err
	}
	return nil
}

func createFinalTarball(
	outpath, inpath string,
	metadata []byte,
	compressionLevel int,
) error {
	fin, err := os.Open(inpath)
	if err != nil {
		return err
	}
	defer fin.Close()

	fout, err := os.Create(outpath)
	if err != nil {
		return err
	}
	defer fout.Close()

	gzout, err := gzip.NewWriterLevel(fout, compressionLevel)
	if err != nil {
		return err
	}

	in := tar.NewReader(fin)
	out := tar.NewWriter(gzout)
	for {
		h, err := in.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if h.Name == "metadata.yaml" {
			// Ignore metadata.yaml, we'll write a new one below.
			continue
		}
		if err := out.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
	}

	writeFile := func(name string, content []byte) error {
		h := &tar.Header{
			Name:     name,
			Mode:     0644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := out.WriteHeader(h); err != nil {
			return err
		}
		_, err = out.Write(content)
		return err
	}
	if err := writeFile("metadata.yaml", metadata); err != nil {
		return err
	}
	for _, t := range cloudInitTemplates {
		if err := writeFile(path.Join("templates", t.Template), []byte(t.content)); err != nil {
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := gzout.Close(); err != nil {
		return err
	}
	return fout.Close()
}

func lxc(args ...string) error {
	return run("lxc", args...)
}

func run(arg0 string, args ...string) error {
	log.Println("Running command:", arg0, strings.Join(args, " "))
	cmd := exec.Command(arg0, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func main() {
	if err := Main(); err != nil {
		log.Fatal(err)
	}
}
