package power

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/net/context"

	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"
	"github.com/docker/engine-api/types/filters"
	"github.com/rancher/os/cmd/control/install"
	"github.com/rancher/os/log"

	"github.com/rancher/os/docker"
	"github.com/rancher/os/util"
)

// You can't shutdown the system from a process in console because we want to stop the console container.
// If you do that you kill yourself.  So we spawn a separate container to do power operations
// This can up because on shutdown we want ssh to gracefully die, terminating ssh connections and not just hanging tcp session
func runDocker(name string) error {
	if os.ExpandEnv("${IN_DOCKER}") == "true" {
		return nil
	}

	client, err := docker.NewSystemClient()
	if err != nil {
		return err
	}

	cmd := []string{name}

	if name == "" {
		name = filepath.Base(os.Args[0])
		cmd = os.Args
	}

	existing, err := client.ContainerInspect(context.Background(), name)
	if err == nil && existing.ID != "" {
		err := client.ContainerRemove(context.Background(), types.ContainerRemoveOptions{
			ContainerID: existing.ID,
		})

		if err != nil {
			return err
		}
	}

	currentContainerID, err := util.GetCurrentContainerID()
	if err != nil {
		return err
	}

	currentContainer, err := client.ContainerInspect(context.Background(), currentContainerID)
	if err != nil {
		return err
	}

	powerContainer, err := client.ContainerCreate(context.Background(),
		&container.Config{
			Image: currentContainer.Config.Image,
			Cmd:   cmd,
			Env: []string{
				"IN_DOCKER=true",
			},
		},
		&container.HostConfig{
			PidMode: "host",
			VolumesFrom: []string{
				currentContainer.ID,
			},
			Privileged: true,
		}, nil, name)
	if err != nil {
		return err
	}

	go func() {
		client.ContainerAttach(context.Background(), types.ContainerAttachOptions{
			ContainerID: powerContainer.ID,
			Stream:      true,
			Stderr:      true,
			Stdout:      true,
		})
	}()

	err = client.ContainerStart(context.Background(), powerContainer.ID)
	if err != nil {
		return err
	}

	_, err = client.ContainerWait(context.Background(), powerContainer.ID)

	if err != nil {
		log.Fatal(err)
	}

	os.Exit(0)
	return nil
}

func reboot(name string, force bool, code uint) {
	if os.Geteuid() != 0 {
		log.Fatalf("%s: Need to be root", os.Args[0])
	}

	// reboot -f should work even when system-docker is having problems
	if !force {
		if kexecFlag || previouskexecFlag || kexecAppendFlag != "" {
			// pass through the cmdline args
			name = ""
		}
		if err := runDocker(name); err != nil {
			log.Fatal(err)
		}
	}

	if kexecFlag || previouskexecFlag || kexecAppendFlag != "" {
		// need to mount boot dir, or `system-docker run -v /:/host -w /host/boot` ?
		baseName := "/mnt/new_img"
		_, _, err := install.MountDevice(baseName, "", "", false)
		if err != nil {
			log.Errorf("ERROR: can't Kexec: %s", err)
			return
		}
		defer util.Unmount(baseName)
		Kexec(previouskexecFlag, filepath.Join(baseName, install.BootDir), kexecAppendFlag)
		return
	}

	if !force {
		err := shutDownContainers()
		if err != nil {
			log.Error(err)
		}
	}

	syscall.Sync()

	err := syscall.Reboot(int(code))
	if err != nil {
		log.Fatal(err)
	}
}

func shutDownContainers() error {
	var err error
	shutDown := true
	timeout := 2
	for i, arg := range os.Args {
		if arg == "-f" || arg == "--f" || arg == "--force" {
			shutDown = false
		}
		if arg == "-t" || arg == "--t" || arg == "--timeout" {
			if len(os.Args) > i+1 {
				t, err := strconv.Atoi(os.Args[i+1])
				if err != nil {
					return err
				}
				timeout = t
			} else {
				log.Error("please specify a timeout")
			}
		}
	}
	if !shutDown {
		return nil
	}
	client, err := docker.NewSystemClient()

	if err != nil {
		return err
	}

	filter := filters.NewArgs()
	filter.Add("status", "running")

	opts := types.ContainerListOptions{
		All:    true,
		Filter: filter,
	}

	containers, err := client.ContainerList(context.Background(), opts)
	if err != nil {
		return err
	}

	currentContainerID, err := util.GetCurrentContainerID()
	if err != nil {
		return err
	}

	var stopErrorStrings []string

	for _, container := range containers {
		if container.ID == currentContainerID {
			continue
		}

		log.Infof("Stopping %s : %v", container.ID[:12], container.Names)
		stopErr := client.ContainerStop(context.Background(), container.ID, timeout)
		if stopErr != nil {
			stopErrorStrings = append(stopErrorStrings, " ["+container.ID+"] "+stopErr.Error())
		}
	}

	var waitErrorStrings []string

	for _, container := range containers {
		if container.ID == currentContainerID {
			continue
		}
		_, waitErr := client.ContainerWait(context.Background(), container.ID)
		if waitErr != nil {
			waitErrorStrings = append(waitErrorStrings, " ["+container.ID+"] "+waitErr.Error())
		}
	}

	if len(waitErrorStrings) != 0 || len(stopErrorStrings) != 0 {
		return errors.New("error while stopping \n1. STOP Errors [" + strings.Join(stopErrorStrings, ",") + "] \n2. WAIT Errors [" + strings.Join(waitErrorStrings, ",") + "]")
	}

	return nil
}
