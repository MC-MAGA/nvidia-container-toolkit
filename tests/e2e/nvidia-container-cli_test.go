/*
 * Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package e2e

import (
	"context"
	"fmt"
	"strings"
	"text/template"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	installDockerTemplate = `
export DEBIAN_FRONTEND=noninteractive

# Add Docker official GPG key:
apt-get update
apt-get install -y ca-certificates curl apt-utils gnupg2
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

# Add the repository to Apt sources:
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo \"${UBUNTU_CODENAME:-$VERSION_CODENAME}\") stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null
apt-get update

apt-get install -y docker-ce docker-ce-cli containerd.io

# start dockerd in the background
dockerd &

# wait for dockerd to be ready with timeout
timeout=30
elapsed=0
while ! docker info > /dev/null 2>&1 && [ $elapsed -lt $timeout ]; do
    echo "Waiting for dockerd to be ready..."
    sleep 1
    elapsed=$((elapsed + 1))
done
if [ $elapsed -ge $timeout ]; then
    echo "Docker failed to start within $timeout seconds"
    exit 1
fi
`
	installCTKTemplate = `
# Create a temporary directory and rootfs path
TMPDIR="$(mktemp -d)"

# Expose TMPDIR for the child namespace
export TMPDIR

docker run --rm -v ${TMPDIR}:/host-tmpdir --entrypoint="sh" {{.ToolkitImage}}-packaging -c "cp -p -R /artifacts/* /host-tmpdir/"
dpkg -i ${TMPDIR}/packages/ubuntu18.04/amd64/libnvidia-container1_*_amd64.deb ${TMPDIR}/packages/ubuntu18.04/amd64/nvidia-container-toolkit-base_*_amd64.deb ${TMPDIR}/packages/ubuntu18.04/amd64/libnvidia-container-tools_*_amd64.deb

nvidia-container-cli --version
`

	libnvidiaContainerCliTestTemplate = `
# Create a temporary directory and rootfs path
TMPDIR="$(mktemp -d)"
ROOTFS="${TMPDIR}/rootfs"
mkdir -p "${ROOTFS}"

# Expose ROOTFS for the child namespace
export ROOTFS TMPDIR

# Download Ubuntu base image with error handling
curl -fsSL http://cdimage.ubuntu.com/ubuntu-base/releases/22.04/release/ubuntu-base-22.04-base-amd64.tar.gz | tar -C $ROOTFS -xz || {
    echo "Failed to download or extract Ubuntu base image"
    exit 1
}

# Enter a new mount + PID namespace so we can pivot_root without touching the
# container'\''s original filesystem.
unshare --mount --pid --fork --propagation private -- sh -eux <<'\''IN_NS'\''
  : "${ROOTFS:?}"

  # 1 Bind-mount the new root and make the mount private
  mount --bind "$ROOTFS" "$ROOTFS"
  mount --make-private "$ROOTFS"
  cd "$ROOTFS"

  # 2 Minimal virtual filesystems
  mount -t proc  proc proc
  mount -t sysfs sys  sys
  mount -t tmpfs tmp  tmp
  mount -t tmpfs run  run

  # 3 Configure NVIDIA devices
  nvidia-container-cli --load-kmods configure --ldconfig=@/sbin/ldconfig.real --no-cgroups --utility --device 0 $(pwd)

  # 4 Switch root into the prepared filesystem
  pivot_root . mnt
  umount -l mnt
  nvidia-smi -L

IN_NS
`

	startTestContainerTemplate = `docker run -d --name {{.ContainerName}} --privileged --runtime=nvidia \
    -e NVIDIA_VISIBLE_DEVICES=runtime.nvidia.com/gpu=all \
    -e NVIDIA_DRIVER_CAPABILITIES=all \
	{{ range $i, $a := .AdditionalArguments -}}
	{{ $a }} \
	{{ end -}}
	ubuntu sleep infinity`
)

var _ = Describe("nvidia-container-cli", Ordered, ContinueOnFailure, Label("libnvidia-container"), func() {
	var (
		runner                       Runner
		containerName                = "node-container-e2e"
		hostOutput                   string
		additionalContainerArguments []string
	)

	BeforeAll(func(ctx context.Context) {
		runner = NewRunner(
			WithHost(sshHost),
			WithPort(sshPort),
			WithSshKey(sshKey),
			WithSshUser(sshUser),
		)

		// Create a tempdir on the runner.
		tmpdir, _, err := runner.Run("mktemp -d --tmpdir=/tmp nvctk-e2e-test-cacheXXX")
		Expect(err).ToNot(HaveOccurred())
		Expect(strings.TrimSpace(tmpdir)).ToNot(BeEmpty())

		localCacheDir = strings.TrimSpace(tmpdir)

		installer, err := NewToolkitInstaller(
			WithToolkitImage(nvidiaContainerToolkitImage),
			WithCacheDir(localCacheDir),
		)
		Expect(err).ToNot(HaveOccurred())

		_, _, err = installer.PrepareCache(runner)
		Expect(err).ToNot(HaveOccurred())

		if installCTK {
			_, _, err = installer.Install(runner)
			Expect(err).ToNot(HaveOccurred())
		} else {
			// If installCTK is false, we use the preinstalled toolkit.
			// TODO: This should be updated for other distributions and other components of the toolkit.
			output, _, err := runner.Run("ls /lib/**/libnvidia-container*.so.*.*")
			Expect(err).ToNot(HaveOccurred())

			output = strings.TrimSpace(output)
			Expect(output).ToNot(BeEmpty())

			for _, lib := range strings.Split(output, "\n") {
				additionalContainerArguments = append(additionalContainerArguments, "-v "+lib+":"+lib)
			}
			additionalContainerArguments = append(additionalContainerArguments,
				"-v /usr/bin/nvidia-container-cli:/usr/bin/nvidia-container-cli",
			)
		}

		// Capture the host GPU list.
		hostOutput, _, err = runner.Run("nvidia-smi -L")
		Expect(err).ToNot(HaveOccurred())

		// Normalize the output once
		hostOutput = strings.TrimSpace(strings.ReplaceAll(hostOutput, "\r", ""))

		// If a container with the same name exists from a previous test run, remove it first.
		// Ignore errors as container might not exist
		runner.Run(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", containerName)) //nolint:errcheck
	})

	AfterAll(func(ctx context.Context) {
		// Cleanup: remove the container and the temporary script on the host.
		// Use || true to ensure cleanup doesn't fail the test
		runner.Run(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", containerName)) //nolint:errcheck
	})

	It("should report the same GPUs inside the container as on the host", func(ctx context.Context) {
		// Launch the container in detached mode.
		var startContainerScriptBuilder strings.Builder
		startContainerTemplate, err := template.New("startContainer").Parse(startTestContainerTemplate)
		Expect(err).ToNot(HaveOccurred())
		err = startContainerTemplate.Execute(&startContainerScriptBuilder, struct {
			ContainerName       string
			AdditionalArguments []string
		}{
			ContainerName:       containerName,
			AdditionalArguments: additionalContainerArguments,
		})
		Expect(err).ToNot(HaveOccurred())

		startContainerScript := startContainerScriptBuilder.String()
		GinkgoLogr.Info("Starting test container", "script", startContainerScript)
		_, _, err = runner.Run(startContainerScript)
		Expect(err).ToNot(HaveOccurred())

		// Install docker in the container.
		_, _, err = runner.Run(fmt.Sprintf("docker exec -u root "+containerName+" bash -c '%s'", installDockerTemplate))
		Expect(err).ToNot(HaveOccurred())

		if installCTK {
			// Install nvidia-container-cli in the container.
			tmpl, err := template.New("toolkitInstall").Parse(installCTKTemplate)
			Expect(err).ToNot(HaveOccurred())

			var toolkitInstall strings.Builder
			err = tmpl.Execute(&toolkitInstall, struct {
				ToolkitImage string
			}{
				ToolkitImage: nvidiaContainerToolkitImage,
			})
			Expect(err).ToNot(HaveOccurred())

			_, _, err = runner.Run(fmt.Sprintf("docker exec -u root "+containerName+" bash -c '%s'", toolkitInstall.String()))
			Expect(err).ToNot(HaveOccurred())
		}

		// Run the test script in the container.
		output, _, err := runner.Run(fmt.Sprintf("docker exec -u root "+containerName+" bash -c '%s'", libnvidiaContainerCliTestTemplate))
		Expect(err).ToNot(HaveOccurred())
		Expect(strings.TrimSpace(output)).ToNot(BeEmpty())
		Expect(hostOutput).To(ContainSubstring(strings.TrimSpace(output)))
	})
})
