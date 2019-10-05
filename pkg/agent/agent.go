package agent

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"os"
	"path"
	"time"

	"github.com/apex/log"
	agent_client "github.com/deviceplane/deviceplane/pkg/agent/client"
	"github.com/deviceplane/deviceplane/pkg/agent/connector"
	"github.com/deviceplane/deviceplane/pkg/agent/handoff"
	"github.com/deviceplane/deviceplane/pkg/agent/info"
	"github.com/deviceplane/deviceplane/pkg/agent/server"
	"github.com/deviceplane/deviceplane/pkg/agent/status"
	"github.com/deviceplane/deviceplane/pkg/agent/supervisor"
	"github.com/deviceplane/deviceplane/pkg/agent/updater"
	"github.com/deviceplane/deviceplane/pkg/agent/variables"
	"github.com/deviceplane/deviceplane/pkg/agent/variables/fsnotify"
	"github.com/deviceplane/deviceplane/pkg/engine"
	"github.com/deviceplane/deviceplane/pkg/file"
	"github.com/deviceplane/deviceplane/pkg/models"
	"github.com/deviceplane/deviceplane/pkg/spec"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

const (
	accessKeyFilename = "access-key"
	deviceIDFilename  = "device-id"
	bundleFilename    = "bundle"
)

var (
	errVersionNotSet = errors.New("version not set")
)

type Agent struct {
	client                 *agent_client.Client // TODO: interface
	variables              variables.Interface
	projectID              string
	registrationToken      string
	confDir                string
	stateDir               string
	supervisor             *supervisor.Supervisor
	statusGarbageCollector *status.GarbageCollector
	connector              *connector.Connector
	infoReporter           *info.Reporter
	server                 *server.Server
	updater                *updater.Updater
	handoffCoordinator     *handoff.Coordinator
}

func NewAgent(
	client *agent_client.Client, engine engine.Engine,
	projectID, registrationToken, confDir, stateDir, version string, serverPort int,
) (*Agent, error) {
	if version == "" {
		return nil, errVersionNotSet
	}
	return &Agent{
		client:            client,
		projectID:         projectID,
		registrationToken: registrationToken,
		confDir:           confDir,
		stateDir:          stateDir,
		supervisor: supervisor.NewSupervisor(engine, func(ctx context.Context, applicationID, currentReleaseID string) error {
			return client.SetDeviceApplicationStatus(ctx, applicationID, models.SetDeviceApplicationStatusRequest{
				CurrentReleaseID: currentReleaseID,
			})
		}, func(ctx context.Context, applicationID, service, currentReleaseID string) error {
			return client.SetDeviceServiceStatus(ctx, applicationID, service, models.SetDeviceServiceStatusRequest{
				CurrentReleaseID: currentReleaseID,
			})
		}),
		statusGarbageCollector: status.NewGarbageCollector(client.DeleteDeviceApplicationStatus, client.DeleteDeviceServiceStatus),
		infoReporter:           info.NewReporter(client, version),
		server:                 server.NewServer(),
		updater:                updater.NewUpdater(engine, projectID, version),
		handoffCoordinator:     handoff.NewCoordinator(engine, version, serverPort),
	}, nil
}

func (a *Agent) fileLocation(elem ...string) string {
	return path.Join(
		append(
			[]string{a.stateDir, a.projectID},
			elem...,
		)...,
	)
}

func (a *Agent) writeFile(contents []byte, elem ...string) error {
	if err := os.MkdirAll(a.fileLocation(), 0700); err != nil {
		return err
	}
	if err := file.WriteFileAtomic(a.fileLocation(elem...), contents, 0644); err != nil {
		return err
	}
	return nil
}

func (a *Agent) Initialize() error {
	if _, err := os.Stat(a.fileLocation(accessKeyFilename)); err == nil {
		log.Info("device already registered")
	} else if os.IsNotExist(err) {
		log.Info("registering device")
		if err = a.register(); err != nil {
			return errors.Wrap(err, "failed to register device")
		}
	} else if err != nil {
		return errors.Wrap(err, "failed to check for access key")
	}

	accessKeyBytes, err := ioutil.ReadFile(a.fileLocation(accessKeyFilename))
	if err != nil {
		return errors.Wrap(err, "failed to read access key")
	}

	deviceIDBytes, err := ioutil.ReadFile(a.fileLocation(deviceIDFilename))
	if err != nil {
		return errors.Wrap(err, "failed to read device ID")
	}

	a.client.SetAccessKey(string(accessKeyBytes))
	a.client.SetDeviceID(string(deviceIDBytes))

	variables := fsnotify.NewVariables(a.confDir)
	if err := variables.Start(); err != nil {
		return errors.Wrap(err, "start fsnotify variables detector")
	}

	a.variables = variables
	a.connector = connector.NewConnector(a.client, a.variables, a.confDir)

	a.server.SetListener(a.handoffCoordinator.Takeover())

	return nil
}

func (a *Agent) register() error {
	registerDeviceResponse, err := a.client.RegisterDevice(context.Background(), a.registrationToken)
	if err != nil {
		return err
	}
	if err := a.writeFile([]byte(registerDeviceResponse.DeviceAccessKeyValue), accessKeyFilename); err != nil {
		return errors.Wrap(err, "failed to save access key")
	}
	if err := a.writeFile([]byte(registerDeviceResponse.DeviceID), deviceIDFilename); err != nil {
		return errors.Wrap(err, "failed to save device ID")
	}
	return nil
}

func (a *Agent) Run() {
	go a.runBundleApplier()
	go a.runConnector()
	go a.runInfoReporter()
	go a.runServer()
	select {}
}

func (a *Agent) runBundleApplier() {
	if bundle := a.loadSavedBundle(); bundle != nil {
		a.supervisor.SetApplications(bundle.Applications)
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if bundle := a.downloadLatestBundle(); bundle != nil {
			a.supervisor.SetApplications(bundle.Applications)
			a.statusGarbageCollector.SetBundle(*bundle)
			var desiredAgentSpec spec.Service
			if err := yaml.Unmarshal([]byte(bundle.DesiredAgentSpec), &desiredAgentSpec); err == nil {
				a.updater.SetDesiredSpec(desiredAgentSpec)
			}
		}

		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) loadSavedBundle() *models.Bundle {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(a.fileLocation(bundleFilename)); err == nil {
			savedBundleBytes, err := ioutil.ReadFile(a.fileLocation(bundleFilename))
			if err != nil {
				log.WithError(err).Error("read saved bundle")
				goto cont
			}

			var savedBundle models.Bundle
			if err = json.Unmarshal(savedBundleBytes, &savedBundle); err != nil {
				log.WithError(err).Error("discarding invalid saved bundle")
				return nil
			}

			return &savedBundle
		} else if os.IsNotExist(err) {
			return nil
		} else {
			log.WithError(err).Error("check if saved bundle exists")
			goto cont
		}

	cont:
		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) downloadLatestBundle() *models.Bundle {
	bundle, err := a.client.GetBundle(context.TODO())
	if err != nil {
		log.WithError(err).Error("get bundle")
		return nil
	}

	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		log.WithError(err).Error("marshal bundle")
		return nil
	}

	if err = a.writeFile(bundleBytes, bundleFilename); err != nil {
		log.WithError(err).Error("save bundle")
		return nil
	}

	return bundle
}

func (a *Agent) runConnector() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		a.connector.Do()

		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) runInfoReporter() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		if err := a.infoReporter.Report(); err != nil {
			log.WithError(err).Error("report device info")
			goto cont
		}

	cont:
		select {
		case <-ticker.C:
			continue
		}
	}
}

func (a *Agent) runServer() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if err := a.server.Serve(); err != nil {
			log.WithError(err).Error("serve device API")
			goto cont
		}

	cont:
		select {
		case <-ticker.C:
			continue
		}
	}
}
