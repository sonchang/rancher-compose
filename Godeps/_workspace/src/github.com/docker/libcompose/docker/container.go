package docker

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/graph/tags"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/utils"
	"github.com/docker/libcompose/logger"
	"github.com/docker/libcompose/project"
	"github.com/samalba/dockerclient"
)

type Container struct {
	project.EmptyService

	name    string
	service *Service
}

func NewContainer(name string, service *Service) *Container {
	return &Container{
		name:    name,
		service: service,
	}
}

func (c *Container) findExisting() (*dockerclient.Container, error) {
	return GetContainerByName(c.service.context.Client, c.name)
}

func (c *Container) Create() (*dockerclient.Container, error) {
	container, err := c.findExisting()
	if err != nil {
		return nil, err
	}

	if container == nil {
		container, err = c.createContainer()
		if err != nil {
			return nil, err
		}
	}

	return container, err
}

func (c *Container) Down() error {
	container, err := c.findExisting()
	if err != nil {
		return err
	}

	if container != nil {
		return c.service.context.Client.StopContainer(container.Id, c.service.context.Timeout)
	}

	return nil
}

func (c *Container) Delete() error {
	container, err := c.findExisting()
	if err != nil || container == nil {
		return err
	}

	info, err := c.service.context.Client.InspectContainer(container.Id)
	if err != nil {
		return err
	}

	if info.State.Running {
		err := c.service.context.Client.StopContainer(container.Id, c.service.context.Timeout)
		if err != nil {
			return err
		}
	}

	return c.service.context.Client.RemoveContainer(container.Id, true, false)
}

func (c *Container) Up() error {
	var err error

	defer func() {
		if err == nil && c.service.context.Log {
			go c.Log()
		}
	}()

	container, err := c.Create()
	if err != nil {
		return err
	}

	info, err := c.service.context.Client.InspectContainer(container.Id)
	if err != nil {
		return err
	}

	if !info.State.Running {
		logrus.Debugf("Starting container: %s: %#v", container.Id, info.HostConfig)
		err = c.populateAdditionalHostConfig(info.HostConfig)
		if err != nil {
			return err
		}
		err := c.service.context.Client.StartContainer(container.Id, info.HostConfig)
		return err
	}

	return nil
}

func (c *Container) createContainer() (*dockerclient.Container, error) {
	config, err := ConvertToApi(c.service.serviceConfig)
	if err != nil {
		return nil, err
	}

	if config.Labels == nil {
		config.Labels = map[string]string{}
	}

	config.Labels[NAME.Str()] = c.name
	config.Labels[SERVICE.Str()] = c.service.name
	config.Labels[PROJECT.Str()] = c.service.context.Project.Name

	err = c.populateAdditionalHostConfig(&config.HostConfig)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("Creating container %s %#v", c.name, config)

	_, err = c.service.context.Client.CreateContainer(config, c.name)
	if err != nil && err.Error() == "Not found" {
		err = c.pull(config.Image)
	}

	if err != nil {
		logrus.Debugf("Failed to create container %s: %v", c.name, err)
		return nil, err
	}

	return c.findExisting()
}

func (c *Container) populateAdditionalHostConfig(hostConfig *dockerclient.HostConfig) error {
	links := map[string]string{}

	for _, link := range c.service.DependentServices() {
		if _, ok := c.service.context.Project.Configs[link.Target]; !ok {
			continue
		}

		service, err := c.service.context.Project.CreateService(link.Target)
		if err != nil {
			return err
		}

		containers, err := service.Containers()
		if err != nil {
			return err
		}

		if link.Type == project.REL_TYPE_LINK {
			c.addLinks(links, service, link, containers)
		} else if link.Type == project.REL_TYPE_IPC_NAMESPACE {
			hostConfig, err = c.addIpc(hostConfig, service, containers)
		} else if link.Type == project.REL_TYPE_NET_NAMESPACE {
			hostConfig, err = c.addNetNs(hostConfig, service, containers)
		}

		if err != nil {
			return err
		}
	}

	hostConfig.Links = []string{}
	for k, v := range links {
		hostConfig.Links = append(hostConfig.Links, strings.Join([]string{v, k}, ":"))
	}

	return nil
}

func (c *Container) addLinks(links map[string]string, service project.Service, rel project.ServiceRelationship, containers []project.Container) {
	for _, container := range containers {
		if _, ok := links[rel.Alias]; !ok {
			links[rel.Alias] = container.Name()
		}

		links[container.Name()] = container.Name()
	}
}

func (c *Container) addIpc(config *dockerclient.HostConfig, service project.Service, containers []project.Container) (*dockerclient.HostConfig, error) {
	if len(containers) == 0 {
		return nil, fmt.Errorf("Failed to find container for IPC %", c.service.Config().Ipc)
	}

	id, err := containers[0].Id()
	if err != nil {
		return nil, err
	}

	config.IpcMode = "container:" + id
	return config, nil
}

func (c *Container) addNetNs(config *dockerclient.HostConfig, service project.Service, containers []project.Container) (*dockerclient.HostConfig, error) {
	if len(containers) == 0 {
		return nil, fmt.Errorf("Failed to find container for networks ns %", c.service.Config().Net)
	}

	id, err := containers[0].Id()
	if err != nil {
		return nil, err
	}

	config.NetworkMode = "container:" + id
	return config, nil
}

func (c *Container) Id() (string, error) {
	container, err := c.findExisting()
	if container == nil {
		return "", err
	} else {
		return container.Id, err
	}
}

func (c *Container) Name() string {
	return c.name
}

func (c *Container) Pull() error {
	return c.pull(c.service.serviceConfig.Image)
}

func (c *Container) Restart() error {
	container, err := c.findExisting()
	if err != nil || container == nil {
		return err
	}

	return c.service.context.Client.RestartContainer(container.Id, c.service.context.Timeout)
}

func (c *Container) Log() error {
	container, err := c.findExisting()
	if container == nil || err != nil {
		return err
	}

	info, err := c.service.context.Client.InspectContainer(container.Id)
	if info == nil || err != nil {
		return err
	}

	l := c.service.context.LoggerFactory.Create(c.name)

	output, err := c.service.context.Client.ContainerLogs(container.Id, &dockerclient.LogOptions{
		Follow: true,
		Stdout: true,
		Stderr: true,
		Tail:   10,
	})
	if err != nil {
		return err
	}

	if info.Config.Tty {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			l.Out([]byte(scanner.Text() + "\n"))
		}
		return scanner.Err()
	} else {
		_, err := stdcopy.StdCopy(&logger.LoggerWrapper{
			Logger: l,
		}, &logger.LoggerWrapper{
			Err:    true,
			Logger: l,
		}, output)
		return err
	}

	return nil
}

func (c *Container) pull(image string) error {
	taglessRemote, tag := parsers.ParseRepositoryTag(image)
	if tag == "" {
		image = utils.ImageReference(taglessRemote, tags.DEFAULTTAG)
	}

	repoInfo, err := registry.ParseRepositoryInfo(taglessRemote)
	if err != nil {
		return err
	}

	authConfig := cliconfig.AuthConfig{}
	if c.service.context.ConfigFile != nil && repoInfo != nil && repoInfo.Index != nil {
		authConfig = registry.ResolveAuthConfig(c.service.context.ConfigFile, repoInfo.Index)
	}

	err = c.service.context.Client.PullImage(image, &dockerclient.AuthConfig{
		Username: authConfig.Username,
		Password: authConfig.Password,
		Email:    authConfig.Email,
	})

	if err != nil {
		logrus.Errorf("Failed to pull image %s: %v", image, err)
	}

	return err
}
