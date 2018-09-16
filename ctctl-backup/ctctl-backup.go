// +build linux,cgo

package main

import (
	"flag"
	"fmt"
	"gopkg.in/lxc/go-lxc.v2"
	"log"
	"os"
	"os/exec"
	"strings"
)

const dirMnt = "/mnt/ctctl-backup"
const snapName = "ctctl-backup"
const volGroup = "VolGroup00"

var (
	lxcpath      string
	backupScript string
)

func init() {
	flag.StringVar(&lxcpath, "lxcpath", lxc.DefaultConfigPath(), "Use specified container path")
	flag.StringVar(&backupScript, "backupScript", "/usr/sbin/ctctl-backup-rsync", "User specified backup script")
	flag.Parse()
}

func main() {
	log.Print("Starting container backup...\n")

	if err := os.MkdirAll(dirMnt, 0700); err != nil {
		log.Fatal("Error creating backup mount dir: ", err)
	}

	cts := lxc.DefinedContainers(lxcpath)
	for _, c := range cts {
		if c.Running() {
			ctName := c.Name()
			ctRootFs := c.RunningConfigItem("lxc.rootfs.path")[0]

			//LXC 2.1 adds "lvm:" to LVM rootfs paths, so remove it.
			ctRootFs = strings.TrimPrefix(ctRootFs, "lvm:")

			//Support pre LXC 2.1.0 configs
			if ctRootFs == "" {
				ctRootFs = c.RunningConfigItem("lxc.rootfs")[0]
			}

			log.Print("Starting phase 1 for ", ctName)
			if err := phase1(ctName, ctRootFs); err != nil {
				log.Print("Phase 1 failed for ", ctName, ": ", err)
				continue
			}

			log.Print("Starting phase 2 for ", ctName)
			if err := phase2(ctName, ctRootFs); err != nil {
				log.Print("Phase 2 failed for ", ctName, ": ", err)
				continue
			}
		}
	}
}

func phase1(ctName string, rootfs string) error {
	//Mount existing LVM
	if err := mountCt(rootfs, dirMnt); err != nil {
		return fmt.Errorf("Could not mount %s: %v", rootfs, err)
	}
	defer umountCt(dirMnt)

	fsTrim(dirMnt)

	//Execute user script for phase 1
	return execUser(ctName, backupScript, dirMnt)
}

func fsTrim(dirMnt string) error {
	cmd := exec.Command("fstrim", dirMnt)
	if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Fstrim failed: %v, %s", err, string(stdoutStderr))
	}

	return nil
}

func execUser(ctName string, backupScript string, dirMnt string) error {
	cmd := exec.Command(backupScript, ctName, dirMnt)
	log.Print("Running backup script: ", backupScript, " ", ctName, " ", dirMnt)
	if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
		fmt.Print(string(stdoutStderr))
		return fmt.Errorf("Cmd failed: %v", err)
	}

	return nil
}

func phase2(ctName string, rootfs string) error {
	snapDev := "/dev/" + volGroup + "/" + snapName

	//Take snapshot of LVM
	if err := snapshotCreate(rootfs); err != nil {
		return fmt.Errorf("Could not snapshot %s: %v", rootfs, err)

	}
	defer snapshotRemove(snapDev)

	//Mount existing LVM
	if err := mountCt(snapDev, dirMnt); err != nil {
		return fmt.Errorf("Could not mount %s: %v", snapDev, err)
	}
	defer umountCt(dirMnt)

	return nil
}

func snapshotCreate(device string) error {
	cmd := exec.Command("lvcreate", "-s", "-L", "1G", "--ignoreactivationskip", "--permission", "r", "-n", snapName, device)
	log.Print("Snapshotting ", device)
	if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Snapshot failed: %v, %s", err, string(stdoutStderr))
	}

	return nil
}

func snapshotRemove(device string) error {
	cmd := exec.Command("lvremove", "-f", device)
	log.Print("Removing snapshot", device)
	if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
		log.Printf("Snapshot removal failed %s: %v, %s", device, err, string(stdoutStderr))
		return fmt.Errorf("Snapshot removal failed: %v, %s", err, string(stdoutStderr))
	}

	return nil
}

func mountCt(device string, dir string) error {
	cmd := exec.Command("mount", "-o", "noexec,noatime", device, dir)
	log.Print("Mounting ", device, " to ", dir)
	if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Mount failed: %v, %s", err, string(stdoutStderr))
	}
	return nil
}

func umountCt(dir string) error {
	cmd := exec.Command("umount", "-f", dir)
	log.Print("Unmounting ", dir)
	if stdoutStderr, err := cmd.CombinedOutput(); err != nil {
		log.Printf("Unmount failed %s: %v, %s", dir, err, string(stdoutStderr))
		return fmt.Errorf("Unmount failed: %v, %s", err, string(stdoutStderr))
	}
	return nil
}
