package ps

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containers/podman/v3/libpod"
	"github.com/containers/podman/v3/libpod/define"
	"github.com/containers/podman/v3/pkg/domain/entities"
	"github.com/containers/podman/v3/pkg/domain/filters"
	psdefine "github.com/containers/podman/v3/pkg/ps/define"
	"github.com/containers/storage"
	"github.com/containers/storage/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func GetContainerLists(runtime *libpod.Runtime, options entities.ContainerListOptions) ([]entities.ListContainer, error) {
	var (
		pss = []entities.ListContainer{}
	)
	filterFuncs := make([]libpod.ContainerFilter, 0, len(options.Filters))
	all := options.All || options.Last > 0
	if len(options.Filters) > 0 {
		for k, v := range options.Filters {
			generatedFunc, err := filters.GenerateContainerFilterFuncs(k, v, runtime)
			if err != nil {
				return nil, err
			}
			filterFuncs = append(filterFuncs, generatedFunc)
		}
	}

	// Docker thinks that if status is given as an input, then we should override
	// the all setting and always deal with all containers.
	if len(options.Filters["status"]) > 0 {
		all = true
	}
	if !all {
		runningOnly, err := filters.GenerateContainerFilterFuncs("status", []string{define.ContainerStateRunning.String()}, runtime)
		if err != nil {
			return nil, err
		}
		filterFuncs = append(filterFuncs, runningOnly)
	}

	cons, err := runtime.GetContainers(filterFuncs...)
	if err != nil {
		return nil, err
	}
	if options.Last > 0 {
		// Sort the libpod containers
		sort.Sort(SortCreateTime{SortContainers: cons})
		// we should perform the lopping before we start getting
		// the expensive information on containers
		if options.Last < len(cons) {
			cons = cons[:options.Last]
		}
	}
	for _, con := range cons {
		listCon, err := ListContainerBatch(runtime, con, options)
		switch {
		case errors.Cause(err) == define.ErrNoSuchCtr:
			continue
		case err != nil:
			return nil, err
		default:
			pss = append(pss, listCon)
		}
	}

	if options.All && options.External {
		listCon, err := GetExternalContainerLists(runtime)
		if err != nil {
			return nil, err
		}
		pss = append(pss, listCon...)
	}

	// Sort the containers we got
	sort.Sort(SortPSCreateTime{SortPSContainers: pss})

	if options.Last > 0 {
		// only return the "last" containers caller requested
		if options.Last < len(pss) {
			pss = pss[:options.Last]
		}
	}
	return pss, nil
}

// GetExternalContainerLists returns list of external containers for e.g. created by buildah
func GetExternalContainerLists(runtime *libpod.Runtime) ([]entities.ListContainer, error) {
	var (
		pss = []entities.ListContainer{}
	)

	externCons, err := runtime.StorageContainers()
	if err != nil {
		return nil, err
	}

	for _, con := range externCons {
		listCon, err := ListStorageContainer(runtime, con)
		switch {
		case errors.Cause(err) == types.ErrLoadError:
			continue
		case err != nil:
			return nil, err
		default:
			pss = append(pss, listCon)
		}
	}
	return pss, nil
}

// ListContainerBatch is used in ps to reduce performance hits by "batching"
// locks.
func ListContainerBatch(rt *libpod.Runtime, ctr *libpod.Container, opts entities.ContainerListOptions) (entities.ListContainer, error) {
	var (
		conConfig                               *libpod.ContainerConfig
		conState                                define.ContainerStatus
		err                                     error
		exitCode                                int32
		exited                                  bool
		pid                                     int
		size                                    *psdefine.ContainerSize
		startedTime                             time.Time
		exitedTime                              time.Time
		cgroup, ipc, mnt, net, pidns, user, uts string
	)

	batchErr := ctr.Batch(func(c *libpod.Container) error {
		if opts.Sync {
			if err := c.Sync(); err != nil {
				return errors.Wrapf(err, "unable to update container state from OCI runtime")
			}
		}

		conConfig = c.Config()
		conState, err = c.State()
		if err != nil {
			return errors.Wrapf(err, "unable to obtain container state")
		}

		exitCode, exited, err = c.ExitCode()
		if err != nil {
			return errors.Wrapf(err, "unable to obtain container exit code")
		}
		startedTime, err = c.StartedTime()
		if err != nil {
			logrus.Errorf("Getting started time for %q: %v", c.ID(), err)
		}
		exitedTime, err = c.FinishedTime()
		if err != nil {
			logrus.Errorf("Getting exited time for %q: %v", c.ID(), err)
		}

		pid, err = c.PID()
		if err != nil {
			return errors.Wrapf(err, "unable to obtain container pid")
		}

		if !opts.Size && !opts.Namespace {
			return nil
		}

		if opts.Namespace {
			ctrPID := strconv.Itoa(pid)
			cgroup, _ = getNamespaceInfo(filepath.Join("/proc", ctrPID, "ns", "cgroup"))
			ipc, _ = getNamespaceInfo(filepath.Join("/proc", ctrPID, "ns", "ipc"))
			mnt, _ = getNamespaceInfo(filepath.Join("/proc", ctrPID, "ns", "mnt"))
			net, _ = getNamespaceInfo(filepath.Join("/proc", ctrPID, "ns", "net"))
			pidns, _ = getNamespaceInfo(filepath.Join("/proc", ctrPID, "ns", "pid"))
			user, _ = getNamespaceInfo(filepath.Join("/proc", ctrPID, "ns", "user"))
			uts, _ = getNamespaceInfo(filepath.Join("/proc", ctrPID, "ns", "uts"))
		}
		if opts.Size {
			size = new(psdefine.ContainerSize)

			rootFsSize, err := c.RootFsSize()
			if err != nil {
				logrus.Errorf("Getting root fs size for %q: %v", c.ID(), err)
			}

			rwSize, err := c.RWSize()
			if err != nil {
				logrus.Errorf("Getting rw size for %q: %v", c.ID(), err)
			}

			size.RootFsSize = rootFsSize
			size.RwSize = rwSize
		}
		return nil
	})
	if batchErr != nil {
		return entities.ListContainer{}, batchErr
	}

	portMappings, err := ctr.PortMappings()
	if err != nil {
		return entities.ListContainer{}, err
	}

	networks, _, err := ctr.Networks()
	if err != nil {
		return entities.ListContainer{}, err
	}

	ps := entities.ListContainer{
		AutoRemove: ctr.AutoRemove(),
		Command:    conConfig.Command,
		Created:    conConfig.CreatedTime,
		Exited:     exited,
		ExitCode:   exitCode,
		ExitedAt:   exitedTime.Unix(),
		ID:         conConfig.ID,
		Image:      conConfig.RootfsImageName,
		ImageID:    conConfig.RootfsImageID,
		IsInfra:    conConfig.IsInfra,
		Labels:     conConfig.Labels,
		Mounts:     ctr.UserVolumes(),
		Names:      []string{conConfig.Name},
		Networks:   networks,
		Pid:        pid,
		Pod:        conConfig.Pod,
		Ports:      portMappings,
		Size:       size,
		StartedAt:  startedTime.Unix(),
		State:      conState.String(),
	}
	if opts.Pod && len(conConfig.Pod) > 0 {
		podName, err := rt.GetName(conConfig.Pod)
		if err != nil {
			if errors.Cause(err) == define.ErrNoSuchCtr {
				return entities.ListContainer{}, errors.Wrapf(define.ErrNoSuchPod, "could not find container %s pod (id %s) in state", conConfig.ID, conConfig.Pod)
			}
			return entities.ListContainer{}, err
		}
		ps.PodName = podName
	}

	if opts.Namespace {
		ps.Namespaces = entities.ListContainerNamespaces{
			Cgroup: cgroup,
			IPC:    ipc,
			MNT:    mnt,
			NET:    net,
			PIDNS:  pidns,
			User:   user,
			UTS:    uts,
		}
	}

	if hc, err := ctr.HealthCheckStatus(); err == nil {
		ps.Status = hc
	} else {
		logrus.Debug(err)
	}

	return ps, nil
}

func ListStorageContainer(rt *libpod.Runtime, ctr storage.Container) (entities.ListContainer, error) {
	name := "unknown"
	if len(ctr.Names) > 0 {
		name = ctr.Names[0]
	}

	ps := entities.ListContainer{
		ID:      ctr.ID,
		Created: ctr.Created,
		ImageID: ctr.ImageID,
		State:   "storage",
		Names:   []string{name},
	}

	buildahCtr, err := rt.IsBuildahContainer(ctr.ID)
	if err != nil {
		return ps, errors.Wrapf(err, "error determining buildah container for container %s", ctr.ID)
	}

	if buildahCtr {
		ps.Command = []string{"buildah"}
	} else {
		ps.Command = []string{"storage"}
	}

	imageName := ""
	if ctr.ImageID != "" {
		image, _, err := rt.LibimageRuntime().LookupImage(ctr.ImageID, nil)
		if err != nil {
			return ps, err
		}
		if len(image.NamesHistory()) > 0 {
			imageName = image.NamesHistory()[0]
		}
	} else if buildahCtr {
		imageName = "scratch"
	}

	ps.Image = imageName
	return ps, nil
}

func getNamespaceInfo(path string) (string, error) {
	val, err := os.Readlink(path)
	if err != nil {
		return "", errors.Wrapf(err, "error getting info from %q", path)
	}
	return getStrFromSquareBrackets(val), nil
}

// getStrFromSquareBrackets gets the string inside [] from a string.
func getStrFromSquareBrackets(cmd string) string {
	reg := regexp.MustCompile(`.*\[|\].*`)
	arr := strings.Split(reg.ReplaceAllLiteralString(cmd, ""), ",")
	return strings.Join(arr, ",")
}

// SortContainers helps us set-up ability to sort by createTime
type SortContainers []*libpod.Container

func (a SortContainers) Len() int      { return len(a) }
func (a SortContainers) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

type SortCreateTime struct{ SortContainers }

func (a SortCreateTime) Less(i, j int) bool {
	return a.SortContainers[i].CreatedTime().After(a.SortContainers[j].CreatedTime())
}

// SortPSContainers helps us set-up ability to sort by createTime
type SortPSContainers []entities.ListContainer

func (a SortPSContainers) Len() int      { return len(a) }
func (a SortPSContainers) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

type SortPSCreateTime struct{ SortPSContainers }

func (a SortPSCreateTime) Less(i, j int) bool {
	return a.SortPSContainers[i].Created.Before(a.SortPSContainers[j].Created)
}
