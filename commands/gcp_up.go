package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	yaml "gopkg.in/yaml.v2"

	"github.com/cloudfoundry/bosh-bootloader/boshinit"
	"github.com/cloudfoundry/bosh-bootloader/cloudconfig/gcp"
	"github.com/cloudfoundry/bosh-bootloader/storage"
)

var (
	writeFile = ioutil.WriteFile
	tempDir   = ioutil.TempDir
	marshal   = yaml.Marshal
)

type GCPUp struct {
	stateStore           stateStore
	keyPairUpdater       keyPairUpdater
	gcpProvider          gcpProvider
	terraformApplier     terraformApplier
	boshDeployer         boshDeployer
	stringGenerator      stringGenerator
	logger               logger
	boshClientProvider   boshClientProvider
	cloudConfigGenerator gcpCloudConfigGenerator
	terraformOutputer    terraformOutputer
}

type GCPUpConfig struct {
	ServiceAccountKeyPath string
	ProjectID             string
	Zone                  string
	Region                string
}

type gcpCloudConfigGenerator interface {
	Generate(gcp.CloudConfigInput) (gcp.CloudConfig, error)
}

type gcpKeyPairCreator interface {
	Create() (string, string, error)
}

type keyPairUpdater interface {
	Update(projectID string) (storage.KeyPair, error)
}

type gcpProvider interface {
	SetConfig(serviceAccountKey string) error
}

type terraformApplier interface {
	Apply(credentials, envID, projectID, zone, region, template, tfState string) (string, error)
}

type terraformOutputer interface {
	Get(tfState, outputName string) (string, error)
}

func NewGCPUp(stateStore stateStore, keyPairUpdater keyPairUpdater, gcpProvider gcpProvider, terraformApplier terraformApplier, boshDeployer boshDeployer,
	stringGenerator stringGenerator, logger logger, boshClientProvider boshClientProvider, cloudConfigGenerator gcpCloudConfigGenerator,
	terraformOutputer terraformOutputer) GCPUp {
	return GCPUp{
		stateStore:           stateStore,
		keyPairUpdater:       keyPairUpdater,
		gcpProvider:          gcpProvider,
		terraformApplier:     terraformApplier,
		boshDeployer:         boshDeployer,
		stringGenerator:      stringGenerator,
		logger:               logger,
		boshClientProvider:   boshClientProvider,
		cloudConfigGenerator: cloudConfigGenerator,
		terraformOutputer:    terraformOutputer,
	}
}

func (u GCPUp) Execute(upConfig GCPUpConfig, state storage.State) error {
	if !upConfig.empty() {
		gcpDetails, err := u.parseUpConfig(upConfig)
		if err != nil {
			return err
		}

		state.IAAS = "gcp"
		state.GCP = gcpDetails
	}

	if err := u.validateState(state); err != nil {
		return err
	}

	if err := u.stateStore.Set(state); err != nil {
		return err
	}

	if err := u.gcpProvider.SetConfig(state.GCP.ServiceAccountKey); err != nil {
		return err
	}

	if state.KeyPair.IsEmpty() {
		keyPair, err := u.keyPairUpdater.Update(state.GCP.ProjectID)
		if err != nil {
			return err
		}
		state.KeyPair = keyPair
		if err := u.stateStore.Set(state); err != nil {
			return err
		}
	}

	tempDir, err := tempDir("", "")
	if err != nil {
		return err
	}

	serviceAccountKeyPath := filepath.Join(tempDir, "credentials.json")
	err = writeFile(serviceAccountKeyPath, []byte(state.GCP.ServiceAccountKey), os.ModePerm)
	if err != nil {
		return err
	}

	tfState, err := u.terraformApplier.Apply(serviceAccountKeyPath, state.EnvID, state.GCP.ProjectID, state.GCP.Zone, state.GCP.Region, terraformTemplate, state.TFState)
	if err != nil {
		return err
	}

	state.TFState = tfState
	if err := u.stateStore.Set(state); err != nil {
		return err
	}

	externalIP, err := u.terraformOutputer.Get(state.TFState, "external_ip")
	if err != nil {
		return err
	}

	networkName, err := u.terraformOutputer.Get(state.TFState, "network_name")
	if err != nil {
		return err
	}
	subnetworkName, err := u.terraformOutputer.Get(state.TFState, "subnetwork_name")
	if err != nil {
		return err
	}
	boshTag, err := u.terraformOutputer.Get(state.TFState, "bosh_open_tag_name")
	if err != nil {
		return err
	}
	internalTag, err := u.terraformOutputer.Get(state.TFState, "internal_tag_name")
	if err != nil {
		return err
	}
	directorAddress, err := u.terraformOutputer.Get(state.TFState, "director_address")
	if err != nil {
		return err
	}

	infrastructureConfiguration := boshinit.InfrastructureConfiguration{
		ElasticIP: externalIP,
		GCP: boshinit.InfrastructureConfigurationGCP{
			Zone:           state.GCP.Zone,
			NetworkName:    networkName,
			SubnetworkName: subnetworkName,
			BOSHTag:        boshTag,
			InternalTag:    internalTag,
			Project:        state.GCP.ProjectID,
			JsonKey:        state.GCP.ServiceAccountKey,
		},
	}

	deployInput, err := boshinit.NewDeployInput(state, infrastructureConfiguration, u.stringGenerator, state.EnvID, "gcp")
	if err != nil {
		return err
	}

	deployOutput, err := u.boshDeployer.Deploy(deployInput)
	if err != nil {
		return err
	}

	if state.BOSH.IsEmpty() {
		state.BOSH = storage.BOSH{
			DirectorName:           deployInput.DirectorName,
			DirectorAddress:        directorAddress,
			DirectorUsername:       deployInput.DirectorUsername,
			DirectorPassword:       deployInput.DirectorPassword,
			DirectorSSLCA:          string(deployOutput.DirectorSSLKeyPair.CA),
			DirectorSSLCertificate: string(deployOutput.DirectorSSLKeyPair.Certificate),
			DirectorSSLPrivateKey:  string(deployOutput.DirectorSSLKeyPair.PrivateKey),
			Credentials:            deployOutput.Credentials,
		}
	}

	state.BOSH.State = deployOutput.BOSHInitState
	state.BOSH.Manifest = deployOutput.BOSHInitManifest

	err = u.stateStore.Set(state)
	if err != nil {
		return err
	}

	boshClient := u.boshClientProvider.Client(state.BOSH.DirectorAddress, state.BOSH.DirectorUsername,
		state.BOSH.DirectorPassword)

	u.logger.Step("generating cloud config")
	azs := u.getAZs(state.GCP.Region)
	cloudConfig, err := u.cloudConfigGenerator.Generate(gcp.CloudConfigInput{
		AZs:            azs,
		Tags:           []string{internalTag},
		NetworkName:    networkName,
		SubnetworkName: subnetworkName,
	})
	if err != nil {
		return err
	}

	manifestYAML, err := marshal(cloudConfig)
	if err != nil {
		return err
	}

	u.logger.Step("applying cloud config")
	if err := boshClient.UpdateCloudConfig(manifestYAML); err != nil {
		return err
	}

	return nil
}

func (u GCPUp) validateState(state storage.State) error {
	switch {
	case state.GCP.ServiceAccountKey == "":
		return errors.New("GCP service account key must be provided")
	case state.GCP.ProjectID == "":
		return errors.New("GCP project ID must be provided")
	case state.GCP.Region == "":
		return errors.New("GCP region must be provided")
	case state.GCP.Zone == "":
		return errors.New("GCP zone must be provided")
	}

	return nil
}

func (u GCPUp) parseUpConfig(upConfig GCPUpConfig) (storage.GCP, error) {
	if upConfig.ServiceAccountKeyPath == "" {
		return storage.GCP{}, errors.New("GCP service account key must be provided")
	}

	sak, err := ioutil.ReadFile(upConfig.ServiceAccountKeyPath)
	if err != nil {
		return storage.GCP{}, fmt.Errorf("error reading service account key: %v", err)
	}

	var tmp interface{}
	err = json.Unmarshal(sak, &tmp)
	if err != nil {
		return storage.GCP{}, fmt.Errorf("error parsing service account key: %v", err)
	}

	return storage.GCP{
		ServiceAccountKey: string(sak),
		ProjectID:         upConfig.ProjectID,
		Zone:              upConfig.Zone,
		Region:            upConfig.Region,
	}, nil
}

func (c GCPUpConfig) empty() bool {
	return c.ServiceAccountKeyPath == "" && c.ProjectID == "" && c.Region == "" && c.Zone == ""
}

func (u GCPUp) getAZs(region string) []string {
	azs := map[string][]string{
		"us-west1":        []string{"us-west1-a", "us-west1-b"},
		"us-central1":     []string{"us-central1-a", "us-central1-b", "us-central1-c", "us-central1-f"},
		"us-east1":        []string{"us-east1-b", "us-east1-c", "us-east1-d"},
		"europe-west1":    []string{"europe-west1-b", "europe-west1-c", "europe-west1-d"},
		"asia-east1":      []string{"asia-east1-a", "asia-east1-b", "asia-east1-c"},
		"asia-northeast1": []string{"asia-northeast1-a", "asia-northeast1-b", "asia-northeast1-c"},
	}
	return azs[region]
}