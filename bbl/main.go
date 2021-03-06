package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io/ioutil"
	"log"
	"os"

	"golang.org/x/crypto/ssh"

	"github.com/cloudfoundry/bosh-bootloader/application"
	"github.com/cloudfoundry/bosh-bootloader/aws"
	"github.com/cloudfoundry/bosh-bootloader/aws/clientmanager"
	"github.com/cloudfoundry/bosh-bootloader/aws/cloudformation"
	"github.com/cloudfoundry/bosh-bootloader/aws/cloudformation/templates"
	"github.com/cloudfoundry/bosh-bootloader/aws/ec2"
	"github.com/cloudfoundry/bosh-bootloader/aws/iam"
	"github.com/cloudfoundry/bosh-bootloader/azure"
	"github.com/cloudfoundry/bosh-bootloader/bosh"
	"github.com/cloudfoundry/bosh-bootloader/certs"
	"github.com/cloudfoundry/bosh-bootloader/cloudconfig"
	"github.com/cloudfoundry/bosh-bootloader/commands"
	"github.com/cloudfoundry/bosh-bootloader/config"
	"github.com/cloudfoundry/bosh-bootloader/gcp"
	"github.com/cloudfoundry/bosh-bootloader/helpers"
	"github.com/cloudfoundry/bosh-bootloader/keypair"
	"github.com/cloudfoundry/bosh-bootloader/proxy"
	"github.com/cloudfoundry/bosh-bootloader/stack"
	"github.com/cloudfoundry/bosh-bootloader/storage"
	"github.com/cloudfoundry/bosh-bootloader/terraform"

	awsapplication "github.com/cloudfoundry/bosh-bootloader/application/aws"
	gcpapplication "github.com/cloudfoundry/bosh-bootloader/application/gcp"
	awscloudconfig "github.com/cloudfoundry/bosh-bootloader/cloudconfig/aws"
	gcpcloudconfig "github.com/cloudfoundry/bosh-bootloader/cloudconfig/gcp"
	awskeypair "github.com/cloudfoundry/bosh-bootloader/keypair/aws"
	gcpkeypair "github.com/cloudfoundry/bosh-bootloader/keypair/gcp"
	awsterraform "github.com/cloudfoundry/bosh-bootloader/terraform/aws"
	gcpterraform "github.com/cloudfoundry/bosh-bootloader/terraform/gcp"
)

var (
	Version     string
	gcpBasePath string
)

func main() {
	newConfig := config.NewConfig(storage.GetState)
	parsedFlags, err := newConfig.Bootstrap(os.Args)
	if err != nil {
		log.Fatalf("\n\n%s\n", err)
	}

	loadedState := parsedFlags.State

	// Utilities
	envIDGenerator := helpers.NewEnvIDGenerator(rand.Reader)
	envGetter := helpers.NewEnvGetter()
	logger := application.NewLogger(os.Stdout)
	stderrLogger := application.NewLogger(os.Stderr)

	// Usage Command
	usage := commands.NewUsage(logger)

	storage.GetStateLogger = stderrLogger

	stateStore := storage.NewStore(parsedFlags.StateDir)
	stateValidator := application.NewStateValidator(parsedFlags.StateDir)

	awsCredentialValidator := awsapplication.NewCredentialValidator(loadedState.AWS.AccessKeyID, loadedState.AWS.SecretAccessKey, loadedState.AWS.Region)
	gcpCredentialValidator := gcpapplication.NewCredentialValidator(loadedState.GCP.ProjectID, loadedState.GCP.ServiceAccountKey, loadedState.GCP.Region, loadedState.GCP.Zone)
	credentialValidator := application.NewCredentialValidator(loadedState.IAAS, gcpCredentialValidator, awsCredentialValidator)

	// Amazon
	awsConfiguration := aws.Config{
		AccessKeyID:     loadedState.AWS.AccessKeyID,
		SecretAccessKey: loadedState.AWS.SecretAccessKey,
		Region:          loadedState.AWS.Region,
	}

	awsClientProvider := &clientmanager.ClientProvider{}
	awsClientProvider.SetConfig(awsConfiguration)

	vpcStatusChecker := ec2.NewVPCStatusChecker(awsClientProvider)
	awsKeyPairCreator := ec2.NewKeyPairCreator(awsClientProvider)
	awsKeyPairDeleter := ec2.NewKeyPairDeleter(awsClientProvider, logger)
	keyPairChecker := ec2.NewKeyPairChecker(awsClientProvider)
	keyPairSynchronizer := ec2.NewKeyPairSynchronizer(awsKeyPairCreator, keyPairChecker, logger)
	awsKeyPairManager := awskeypair.NewManager(keyPairSynchronizer, awsKeyPairDeleter, awsClientProvider)
	awsAvailabilityZoneRetriever := ec2.NewAvailabilityZoneRetriever(awsClientProvider)
	templateBuilder := templates.NewTemplateBuilder(logger)
	stackManager := cloudformation.NewStackManager(awsClientProvider, logger)
	infrastructureManager := cloudformation.NewInfrastructureManager(templateBuilder, stackManager)
	certificateDescriber := iam.NewCertificateDescriber(awsClientProvider)
	certificateDeleter := iam.NewCertificateDeleter(awsClientProvider)
	certificateValidator := certs.NewValidator()
	userPolicyDeleter := iam.NewUserPolicyDeleter(awsClientProvider)

	// GCP
	gcpClientProvider := gcp.NewClientProvider(gcpBasePath)
	if loadedState.IAAS == "gcp" {
		err = gcpClientProvider.SetConfig(loadedState.GCP.ServiceAccountKey, loadedState.GCP.ProjectID, loadedState.GCP.Region, loadedState.GCP.Zone)
		if err != nil {
			log.Fatalf("\n\n%s\n", err)
		}
	}
	gcpKeyPairUpdater := gcp.NewKeyPairUpdater(rand.Reader, rsa.GenerateKey, ssh.NewPublicKey, gcpClientProvider.Client(), logger)
	gcpKeyPairDeleter := gcp.NewKeyPairDeleter(gcpClientProvider.Client(), logger)
	gcpNetworkInstancesChecker := gcp.NewNetworkInstancesChecker(gcpClientProvider.Client())
	gcpKeyPairManager := gcpkeypair.NewManager(gcpKeyPairUpdater, gcpKeyPairDeleter)

	// EnvID
	envIDManager := helpers.NewEnvIDManager(envIDGenerator, gcpClientProvider.Client(), infrastructureManager)

	// Keypair Manager
	keyPairManager := keypair.NewManager(awsKeyPairManager, gcpKeyPairManager)

	// Terraform
	terraformOutputBuffer := bytes.NewBuffer([]byte{})

	terraformCmd := terraform.NewCmd(os.Stderr, terraformOutputBuffer)
	terraformExecutor := terraform.NewExecutor(terraformCmd, parsedFlags.Debug)
	gcpTemplateGenerator := gcpterraform.NewTemplateGenerator()
	gcpInputGenerator := gcpterraform.NewInputGenerator()
	gcpOutputGenerator := gcpterraform.NewOutputGenerator(terraformExecutor)
	awsTemplateGenerator := awsterraform.NewTemplateGenerator()
	awsInputGenerator := awsterraform.NewInputGenerator(awsAvailabilityZoneRetriever)
	awsOutputGenerator := awsterraform.NewOutputGenerator(terraformExecutor)
	templateGenerator := terraform.NewTemplateGenerator(gcpTemplateGenerator, awsTemplateGenerator)
	inputGenerator := terraform.NewInputGenerator(gcpInputGenerator, awsInputGenerator)
	stackMigrator := stack.NewMigrator(terraformExecutor, infrastructureManager, certificateDescriber, userPolicyDeleter, awsAvailabilityZoneRetriever)
	terraformManager := terraform.NewManager(terraform.NewManagerArgs{
		Executor:              terraformExecutor,
		TemplateGenerator:     templateGenerator,
		InputGenerator:        inputGenerator,
		AWSOutputGenerator:    awsOutputGenerator,
		GCPOutputGenerator:    gcpOutputGenerator,
		TerraformOutputBuffer: terraformOutputBuffer,
		Logger:                logger,
		StackMigrator:         stackMigrator,
	})

	// BOSH
	hostKeyGetter := proxy.NewHostKeyGetter()
	socks5Proxy := proxy.NewSocks5Proxy(logger, hostKeyGetter, 0)
	boshCommand := bosh.NewCmd(os.Stderr)
	boshExecutor := bosh.NewExecutor(boshCommand, ioutil.TempDir, ioutil.ReadFile, json.Unmarshal,
		json.Marshal, ioutil.WriteFile)
	boshManager := bosh.NewManager(boshExecutor, logger, socks5Proxy)
	boshClientProvider := bosh.NewClientProvider()

	// Environment Validators
	awsBrokenEnvironmentValidator := awsapplication.NewBrokenEnvironmentValidator(infrastructureManager)
	awsEnvironmentValidator := awsapplication.NewEnvironmentValidator(infrastructureManager, boshClientProvider)

	// Cloud Config
	sshKeyGetter := bosh.NewSSHKeyGetter()
	awsCloudFormationOpsGenerator := awscloudconfig.NewCloudFormationOpsGenerator(awsAvailabilityZoneRetriever, infrastructureManager)
	awsTerraformOpsGenerator := awscloudconfig.NewTerraformOpsGenerator(terraformManager)
	gcpOpsGenerator := gcpcloudconfig.NewOpsGenerator(terraformManager)
	cloudConfigOpsGenerator := cloudconfig.NewOpsGenerator(awsCloudFormationOpsGenerator, awsTerraformOpsGenerator, gcpOpsGenerator)
	cloudConfigManager := cloudconfig.NewManager(logger, boshCommand, cloudConfigOpsGenerator, boshClientProvider, socks5Proxy, terraformManager, sshKeyGetter)

	// Subcommands
	awsUp := commands.NewAWSUp(
		awsCredentialValidator, keyPairManager, boshManager,
		cloudConfigManager, stateStore, awsClientProvider, envIDManager, terraformManager, awsBrokenEnvironmentValidator)

	awsCreateLBs := commands.NewAWSCreateLBs(
		logger, awsCredentialValidator, cloudConfigManager,
		stateStore, terraformManager, awsEnvironmentValidator,
	)

	awsLBs := commands.NewAWSLBs(terraformManager, logger)

	awsUpdateLBs := commands.NewAWSUpdateLBs(awsCreateLBs, awsCredentialValidator, awsEnvironmentValidator)

	awsDeleteLBs := commands.NewAWSDeleteLBs(
		awsCredentialValidator, logger, cloudConfigManager, stateStore, awsEnvironmentValidator,
		terraformManager,
	)

	azureClient := azure.NewClient()
	azureUp := commands.NewAzureUp(azureClient, logger)

	gcpDeleteLBs := commands.NewGCPDeleteLBs(stateStore, terraformManager, cloudConfigManager)

	gcpUp := commands.NewGCPUp(commands.NewGCPUpArgs{
		StateStore:                   stateStore,
		KeyPairManager:               keyPairManager,
		TerraformManager:             terraformManager,
		BoshManager:                  boshManager,
		Logger:                       logger,
		EnvIDManager:                 envIDManager,
		CloudConfigManager:           cloudConfigManager,
		GCPAvailabilityZoneRetriever: gcpClientProvider.Client(),
	})

	gcpCreateLBs := commands.NewGCPCreateLBs(terraformManager, cloudConfigManager, stateStore, logger, gcpClientProvider.Client())

	gcpLBs := commands.NewGCPLBs(terraformManager, logger)

	gcpUpdateLBs := commands.NewGCPUpdateLBs(gcpCreateLBs)

	// Commands
	commandSet := application.CommandSet{}
	commandSet["help"] = usage
	commandSet["version"] = commands.NewVersion(Version, logger)
	commandSet["up"] = commands.NewUp(awsUp, gcpUp, azureUp, envGetter, boshManager)
	commandSet["destroy"] = commands.NewDestroy(
		credentialValidator, logger, os.Stdin, boshManager, vpcStatusChecker, stackManager,
		infrastructureManager, awsKeyPairDeleter, gcpKeyPairDeleter, certificateDeleter,
		stateStore, stateValidator, terraformManager, gcpNetworkInstancesChecker,
	)
	commandSet["down"] = commandSet["destroy"]
	commandSet["create-lbs"] = commands.NewCreateLBs(awsCreateLBs, gcpCreateLBs, stateValidator, certificateValidator, boshManager)
	commandSet["update-lbs"] = commands.NewUpdateLBs(awsUpdateLBs, gcpUpdateLBs, certificateValidator, stateValidator, logger, boshManager)
	commandSet["delete-lbs"] = commands.NewDeleteLBs(gcpDeleteLBs, awsDeleteLBs, logger, stateValidator, boshManager)
	commandSet["lbs"] = commands.NewLBs(gcpLBs, awsLBs, stateValidator, logger)
	commandSet["jumpbox-address"] = commands.NewStateQuery(logger, stateValidator, terraformManager, infrastructureManager, commands.JumpboxAddressPropertyName)
	commandSet["director-address"] = commands.NewStateQuery(logger, stateValidator, terraformManager, infrastructureManager, commands.DirectorAddressPropertyName)
	commandSet["director-username"] = commands.NewStateQuery(logger, stateValidator, terraformManager, infrastructureManager, commands.DirectorUsernamePropertyName)
	commandSet["director-password"] = commands.NewStateQuery(logger, stateValidator, terraformManager, infrastructureManager, commands.DirectorPasswordPropertyName)
	commandSet["director-ca-cert"] = commands.NewStateQuery(logger, stateValidator, terraformManager, infrastructureManager, commands.DirectorCACertPropertyName)
	commandSet["ssh-key"] = commands.NewSSHKey(logger, stateValidator, sshKeyGetter)
	commandSet["env-id"] = commands.NewStateQuery(logger, stateValidator, terraformManager, infrastructureManager, commands.EnvIDPropertyName)
	commandSet["latest-error"] = commands.NewLatestError(logger, stateValidator)
	commandSet["print-env"] = commands.NewPrintEnv(logger, stateValidator, terraformManager)
	commandSet["cloud-config"] = commands.NewCloudConfig(logger, stateValidator, cloudConfigManager)
	commandSet["bosh-deployment-vars"] = commands.NewBOSHDeploymentVars(logger, boshManager, stateValidator, terraformManager)
	commandSet["rotate"] = commands.NewRotate(stateStore, keyPairManager, terraformManager, boshManager, stateValidator)

	commandConfiguration := &application.Configuration{
		Global: application.GlobalConfiguration{
			StateDir: parsedFlags.StateDir,
			Debug:    parsedFlags.Debug,
		},
		State:           loadedState,
		ShowCommandHelp: parsedFlags.Help,
	}

	if len(parsedFlags.RemainingArgs) > 0 {
		commandConfiguration.Command = parsedFlags.RemainingArgs[0]
		commandConfiguration.SubcommandFlags = parsedFlags.RemainingArgs[1:]
	} else {
		commandConfiguration.ShowCommandHelp = false
		if parsedFlags.Help {
			commandConfiguration.Command = "help"
		}
		if parsedFlags.Version {
			commandConfiguration.Command = "version"
		}
	}

	if len(os.Args) == 1 {
		commandConfiguration.Command = "help"
	}

	app := application.New(commandSet, *commandConfiguration, usage)

	err = app.Run()
	if err != nil {
		log.Fatalf("\n\n%s\n", err)
	}
}
