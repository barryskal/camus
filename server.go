package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"
	"time"
)

type Deploy struct {
	Id   string
	Note string
	Port int // -1 for not running
}

type Label string

type Server interface {
	ListLabels() ([]Label, error)
	ListDeploys() ([]Deploy, error)
	Run(deployId string) error
	Stop(deployId string) error
	Label(deployId string, label Label) error

	// TODO Maintenance mode
}

const (
	deployPath = "deploys"
	configPath = "config.json"
)

type Config struct {
	Ports  map[int]string
	Labels map[string]string
}

type ServerImpl struct {
	root   string
	config *Config
}

func readConfig(path string) (*Config, error) {
	var config Config
	if data, err := ioutil.ReadFile(path); err == nil {
		err = json.Unmarshal(data, &config)
		if err != nil {
			return nil, err
		}
	}
	if config.Ports == nil {
		config.Ports = make(map[int]string)
	}
	if config.Labels == nil {
		config.Labels = make(map[string]string)
	}
	return &config, nil
}

func NewServerImpl(root string) (*ServerImpl, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		log.Fatal("Root path:", err)
	}
	config, err := readConfig(path.Join(root, configPath))
	if err != nil {
		return nil, err
	}
	if _, err = os.Open(path.Join(root, deployPath)); os.IsNotExist(err) {
		os.MkdirAll(path.Join(root, deployPath), 0644)
	}
	return &ServerImpl{root, config}, nil
}

func (s *ServerImpl) NewDeployDir() NewDeployDirResponse {
	t := time.Now()
	timestamp := fmt.Sprintf("%d-%02d-%02d-%02d-%02d-%02d",
		t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())

	return NewDeployDirResponse{
		DeployId: timestamp,
		Path:     path.Join(s.root, deployPath, timestamp),
	}
}

func (s *ServerImpl) ListDeploys() ([]Deploy, error) {
	infos, err := ioutil.ReadDir(path.Join(s.root, deployPath))
	if err != nil {
		return nil, err
	}
	var result []Deploy
	for _, info := range infos {
		result = append(result, Deploy{
			Id:   info.Name(),
			Port: -1,
		})
	}
	return result, nil
}

func (s *ServerImpl) findUnusedPort() (int, error) {
	for i := 8001; i < 8100; i++ {

		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", i))
		if err != nil {
			// TODO: Is this now safe to assume the port is free?
			// NOTE(dan): I tried implementing listening on the port
			// instead, but it always succeeded even if there was
			// actually something already there...
			return i, nil
		} else {
			conn.Close()
		}

	}

	return -1, errors.New("Could not find free port")
}

func (s *ServerImpl) writeConfig() error {
	data, err := json.Marshal(s.config)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path.Join(s.root, configPath), data, os.FileMode(0644))
}

func (s *ServerImpl) Run(deployId string) error {
	port, err := s.findUnusedPort()
	if err != nil {
		return err
	}
	log.Println("Found port ", port)

	deployPath := path.Join(s.root, deployPath, deployId)

	app, err := ApplicationFromConfig(path.Join(deployPath, "deploy.json"))
	if err != nil {
		return err
	}

	s.config.Ports[port] = deployId
	s.writeConfig()
	println(deployPath)
	println(app.RunCmd(port))
	cmd := exec.Command("sh", "-c", app.RunCmd(port))

	// process working dir
	cmd.Dir = deployPath

	// give it its own process group, so it doesn't die
	// when the manager process exits for whatever reason
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	cmd.SysProcAttr.Setpgid = true

	err = cmd.Start()
	if err != nil {
		return err
	}

	return waitForAppToStart(port, app)
}

var MAX_STARTUP_TIME = time.Duration( /* XXX XXX */ 1) * time.Second
var MAX_HEALTH_CHECK_TIME = time.Duration(2) * time.Second
var STARTUP_HEALTH_CHECK_INTERVAL = time.Duration(100) * time.Millisecond

func waitForAppToStart(port int, app Application) error {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("health check should not redirect")
		},
		Timeout: MAX_HEALTH_CHECK_TIME,
	}

	end := time.Now().Add(MAX_STARTUP_TIME)
	for {
		log.Print(".")

		resp, err := client.Get(
			fmt.Sprintf("http://localhost:%d%s", port, app.HealthEndpoint()))

		if err == nil {
			if resp.StatusCode == 200 {
				log.Println("ok")
				return nil
			} else {
				log.Println("bad:", resp.StatusCode)
				return errors.New(fmt.Sprintf("Health check failed %d", resp.StatusCode))
			}
		}

		if time.Now().After(end) {
			return errors.New("Failed to connect to app after timeout")
		}

		time.Sleep(STARTUP_HEALTH_CHECK_INTERVAL)
	}
}
