package base

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	dt "github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/opendexnetwork/opendex-docker/launcher/log"
	"github.com/opendexnetwork/opendex-docker/launcher/service"
	"github.com/opendexnetwork/opendex-docker/launcher/types"
	"github.com/opendexnetwork/opendex-docker/launcher/utils"
	"github.com/sirupsen/logrus"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Service struct {
	Name    string
	Context types.Context

	Hostname    string
	Image       string
	Command     []string
	Environment map[string]string
	Ports       []string
	Volumes     []string
	Disabled    bool
	DataDir     string

	client        *docker.Client
	ContainerName string

	Logger *logrus.Entry
}

func New(ctx types.Context, name string) (*Service, error) {
	client, err := docker.NewClientWithOpts(docker.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %s", err)
	}

	return &Service{
		Name:          name,
		Context:       ctx,
		client:        client,
		ContainerName: fmt.Sprintf("%s_%s_1", ctx.GetNetwork(), name),
		Logger:        log.NewLogger(fmt.Sprintf("service.%s", name)),

		Hostname:    name,
		Image:       "",
		Command:     []string{},
		Environment: make(map[string]string),
		Ports:       []string{},
		Volumes:     []string{},
		Disabled:    false,
		DataDir:     "",
	}, nil
}

func (t *Service) GetName() string {
	return t.Name
}

func (t *Service) GetStatus(ctx context.Context) (string, error) {
	c, err := t.getContainer(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Container %s", c.State.Status), nil
}

func (t *Service) Start(ctx context.Context) error {
	if err := t.client.ContainerStart(ctx, t.ContainerName, dt.ContainerStartOptions{}); err != nil {
		return err
	}
	return nil
}

func (t *Service) Stop(ctx context.Context) error {
	if err := t.client.ContainerStop(ctx, t.ContainerName, nil); err != nil {
		return err
	}
	return nil
}

func (t *Service) Restart(ctx context.Context) error {
	if err := t.client.ContainerRestart(ctx, t.ContainerName, nil); err != nil {
		return err
	}
	return nil
}

func (t *Service) Create(ctx context.Context) error {
	c := exec.Command("docker-compose", "up", "-d", "--no-start", t.Name)
	return utils.Run(ctx, c)
}

func (t *Service) Up(ctx context.Context) error {
	c := exec.Command("docker-compose", "up", "-d", t.Name)
	return utils.Run(ctx, c)
}

func (t *Service) demuxLogsReader(reader io.Reader) io.Reader {
	r, w := io.Pipe()
	go func() {
		stdcopy.StdCopy(w, w, reader)
		w.Close()
	}()
	return r
}

func (t *Service) GetLogs(ctx context.Context, since string, tail string) ([]string, error) {
	reader, err := t.client.ContainerLogs(ctx, t.ContainerName, dt.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      since,
		Tail:       tail,
		Follow:     false,
	})
	if err != nil {
		return nil, err
	}

	var lines []string
	r := t.demuxLogsReader(reader)

	bufReader := bufio.NewReader(r)
	for {
		line, _, err := bufReader.ReadLine()
		if err != nil {
			break
		}
		lines = append(lines, string(line))
	}

	return lines, nil
}

func (t *Service) FollowLogs(ctx context.Context, since string, tail string) (<-chan string, func(), error) {
	reader, err := t.client.ContainerLogs(ctx, t.ContainerName, dt.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      since,
		Tail:       tail,
		Follow:     true,
	})
	if err != nil {
		return nil, nil, err
	}

	r := t.demuxLogsReader(reader)

	ch := make(chan string)

	go func() {
		bufReader := bufio.NewReader(r)
		for {
			line, _, err := bufReader.ReadLine()
			if err != nil {
				ch <- "--- EOF ---"
				break
			}
			ch <- string(line)
		}
		close(ch)
	}()

	return ch, func() { reader.Close() }, nil
}

func (t *Service) Exec(ctx context.Context, name string, args ...string) (string, error) {
	createResp, err := t.client.ContainerExecCreate(ctx, t.ContainerName, dt.ExecConfig{
		Cmd:          append([]string{name}, args...),
		Tty:          false,
		AttachStdin:  false,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("[docker] create exec: %w", err)
	}

	execId := createResp.ID

	// ContainerExecAttach = ContainerExecStart
	attachResp, err := t.client.ContainerExecAttach(ctx, execId, dt.ExecStartCheck{
		Detach: false,
		Tty:    false,
	})
	if err != nil {
		return "", fmt.Errorf("[docker] attach exec: %w", err)
	}

	var buf bytes.Buffer
	_, err = stdcopy.StdCopy(&buf, &buf, attachResp.Reader)
	if err != nil {
		return "", fmt.Errorf("[docker] stdcopy: %w", err)
	}

	exec_, err := t.client.ContainerExecInspect(ctx, execId)
	if err != nil {
		return "", fmt.Errorf("[docker] inspect exec: %w", err)
	}
	exitCode := exec_.ExitCode

	if exitCode != 0 {
		output := buf.String()
		msg := fmt.Sprintf("[docker] command \"%s\" exits with non-zero code %d: %s", strings.Join(append([]string{name}, args...), " "), exitCode, strings.TrimSpace(output))
		return "", service.ErrExec{
			Output:   output,
			ExitCode: exitCode,
			Message:  msg,
		}
	}

	return buf.String(), nil
}

func (t *Service) Apply(cfg interface{}) error {
	c := cfg.(Config)

	t.Image = c.Image
	t.Ports = c.ExposePorts
	t.Disabled = c.Disabled
	t.Environment = map[string]string{}
	t.Environment["NETWORK"] = string(t.Context.GetNetwork())
	t.DataDir = c.Dir
	t.Ports = []string{}
	t.Volumes = []string{}
	t.Command = []string{}

	return nil
}

func (t *Service) GetImage() string {
	return t.Image
}

func (t *Service) GetHostname() string {
	return t.Hostname
}

func (t *Service) GetCommand() []string {
	return t.Command
}

func (t *Service) GetEnvironment() map[string]string {
	return t.Environment
}

func (t *Service) GetPorts() []string {
	return t.Ports
}

func (t *Service) GetVolumes() []string {
	return t.Volumes
}

func (t *Service) IsDisabled() bool {
	return t.Disabled
}

func (t *Service) GetRpcParams() (interface{}, error) {
	return make(map[string]interface{}), nil
}

func (t *Service) GetDefaultConfig() interface{} {
	return nil
}

func (t *Service) getContainer(ctx context.Context) (*dt.ContainerJSON, error) {
	c, err := t.client.ContainerInspect(ctx, t.ContainerName)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (t *Service) GetStartedAt(ctx context.Context) (string, error) {
	c, err := t.getContainer(ctx)
	if err != nil {
		return "", err
	}
	return c.State.StartedAt, nil
}

func (t *Service) GetDataDir() string {
	return t.DataDir
}

func (t *Service) GetMode() string {
	return ""
}

func (t *Service) Rescue(ctx context.Context) bool {
	return true
}

func (t *Service) Remove(ctx context.Context) error {
	return t.client.ContainerRemove(ctx, t.ContainerName, dt.ContainerRemoveOptions{
		RemoveVolumes: true,
		RemoveLinks:   false,
		Force:         true,
	})
}

func (t *Service) RemoveData(ctx context.Context) error {
	err := os.RemoveAll(t.DataDir)
	if err != nil {
		t.Logger.Warnf("Forcefully remove %s", t.DataDir)
		cmd := exec.Command("sudo", "rm", "-rf", t.DataDir)
		return cmd.Run()
	}
	return nil
}

func (t *Service) IsRunning() bool {
	status, err := t.GetStatus(context.Background())
	if err != nil {
		return false
	}
	return status == "Container running"
}
