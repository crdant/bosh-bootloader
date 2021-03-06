package commands

import (
	"errors"
	"fmt"

	"github.com/cloudfoundry/bosh-bootloader/storage"
)

const (
	EnvIDCommand            = "env-id"
	JumpboxAddressCommand   = "jumpbox-address"
	DirectorUsernameCommand = "director-username"
	DirectorPasswordCommand = "director-password"
	DirectorAddressCommand  = "director-address"
	DirectorCACertCommand   = "director-ca-cert"

	EnvIDPropertyName            = "environment id"
	JumpboxAddressPropertyName   = "jumpbox address"
	DirectorUsernamePropertyName = "director username"
	DirectorPasswordPropertyName = "director password"
	DirectorAddressPropertyName  = "director address"
	DirectorCACertPropertyName   = "director ca cert"
)

type StateQuery struct {
	logger                logger
	stateValidator        stateValidator
	terraformManager      terraformOutputter
	infrastructureManager infrastructureManager
	propertyName          string
}

type getPropertyFunc func(storage.State) string

func NewStateQuery(logger logger, stateValidator stateValidator, terraformManager terraformOutputter, infrastructureManager infrastructureManager, propertyName string) StateQuery {
	return StateQuery{
		logger:                logger,
		stateValidator:        stateValidator,
		terraformManager:      terraformManager,
		infrastructureManager: infrastructureManager,
		propertyName:          propertyName,
	}
}

func (s StateQuery) CheckFastFails(subcommandFlags []string, state storage.State) error {
	err := s.stateValidator.Validate()
	if err != nil {
		return err
	}

	if state.NoDirector && s.propertyName != DirectorAddressPropertyName && s.propertyName != EnvIDPropertyName {
		return errors.New("Error BBL does not manage this director.")
	}

	return nil
}

func (s StateQuery) Execute(subcommandFlags []string, state storage.State) error {
	var propertyValue string
	switch s.propertyName {
	case JumpboxAddressPropertyName:
		if state.Jumpbox.Enabled {
			propertyValue = state.Jumpbox.URL
		}
	case DirectorAddressPropertyName:
		if !state.NoDirector {
			propertyValue = state.BOSH.DirectorAddress
		} else {
			externalIP, err := s.getEIP(state)
			if err != nil {
				return err
			}
			propertyValue = fmt.Sprintf("https://%s:25555", externalIP)
		}
	case DirectorUsernamePropertyName:
		propertyValue = state.BOSH.DirectorUsername
	case DirectorPasswordPropertyName:
		propertyValue = state.BOSH.DirectorPassword
	case DirectorCACertPropertyName:
		propertyValue = state.BOSH.DirectorSSLCA
	case EnvIDPropertyName:
		propertyValue = state.EnvID
	}

	if propertyValue == "" {
		return fmt.Errorf("Could not retrieve %s, please make sure you are targeting the proper state dir.", s.propertyName)
	}

	s.logger.Println(propertyValue)
	return nil
}

func (s StateQuery) getEIP(state storage.State) (string, error) {
	switch state.IAAS {
	case "aws":
		stack, err := s.infrastructureManager.Describe(state.Stack.Name)
		if err != nil {
			return "", err
		}
		return stack.Outputs["BOSHEIP"], nil
	case "gcp":
		terraformOutputs, err := s.terraformManager.GetOutputs(state)
		if err != nil {
			return "", err
		}

		return terraformOutputs["external_ip"].(string), nil
	}

	return "", errors.New("Could not find external IP for given IAAS")
}
