// Copyright (c) The Amphitheatre Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buildpacks/libcnb"
	"github.com/paketo-buildpacks/libpak"
	"github.com/paketo-buildpacks/libpak/bard"
	"github.com/paketo-buildpacks/libpak/crush"
	"github.com/paketo-buildpacks/libpak/effect"
	"github.com/paketo-buildpacks/libpak/sherpa"
)

type Sui struct {
	LayerContributor libpak.DependencyLayerContributor
	configResolver   libpak.ConfigurationResolver
	Logger           bard.Logger
	Executor         effect.Executor
}

type DeployWallet struct {
	Address   string `json:"suiAddress"`
	KeyScheme string `json:"keyScheme"`
}

func NewSui(dependency libpak.BuildpackDependency, cache libpak.DependencyCache, configResolver libpak.ConfigurationResolver) Sui {
	contributor := libpak.NewDependencyLayerContributor(dependency, cache, libcnb.LayerTypes{
		Build:  true,
		Cache:  true,
		Launch: true,
	})
	return Sui{
		LayerContributor: contributor,
		configResolver:   configResolver,
		Executor:         effect.NewExecutor(),
	}
}

func (r Sui) Contribute(layer libcnb.Layer) (libcnb.Layer, error) {
	r.LayerContributor.Logger = r.Logger
	return r.LayerContributor.Contribute(layer, func(artifact *os.File) (libcnb.Layer, error) {
		moveHome := filepath.Join(layer.Path, "move")
		suiConfig := filepath.Join(layer.Path, "sui-config")

		bin := filepath.Join(layer.Path, "bin")
		binFile := filepath.Join(bin, PlanEntrySui)

		tempDir := os.TempDir()
		r.Logger.Bodyf("Expanding %s to %s", artifact.Name(), tempDir)
		if err := crush.Extract(artifact, tempDir, 1); err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to expand %s\n%w", artifact.Name(), err)
		}

		originBinFile := filepath.Join(tempDir, "target", "release", fmt.Sprintf("%s-ubuntu-x86_64", PlanEntrySui))
		r.Logger.Bodyf("Copying %s to %s", originBinFile, binFile)
		originBin, err := os.Open(originBinFile)
		if err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to open %s\n%w", originBinFile, err)
		}
		defer originBin.Close()

		if err := sherpa.CopyFile(originBin, binFile); err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to copy %s to %s\n%w", originBinFile, binFile, err)
		}

		// Must be set to executable
		r.Logger.Bodyf("Setting %s as executable", binFile)
		if err := os.Chmod(binFile, 0755); err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to chmod %s\n%w", binFile, err)
		}

		// Must be set to PATH
		r.Logger.Bodyf("Setting %s in PATH", bin)
		if err := os.Setenv("PATH", sherpa.AppendToEnvVar("PATH", ":", bin)); err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to set $PATH\n%w", err)
		}

		// get sui version
		buf, err := r.Execute(PlanEntrySui, []string{"--version"})
		if err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to get %s version\n%w", PlanEntrySui, err)
		}
		version := strings.Split(strings.TrimSpace(buf.String()), " ")[1]
		r.Logger.Bodyf("Checking %s version: %s", PlanEntrySui, version)

		// set MOVE_HOME
		r.Logger.Bodyf("Setting MOVE_HOME=%s", moveHome)
		if err := os.Setenv("MOVE_HOME", moveHome); err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to set MOVE_HOME\n%w", err)
		}

		// set SUI_CONFIG_DIR
		r.Logger.Bodyf("Setting SUI_CONFIG_DIR=%s", suiConfig)
		if err := os.Setenv("SUI_CONFIG_DIR", suiConfig); err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to set SUI_CONFIG_DIR\n%w", err)
		}

		// compile contract
		args := []string{"move", "build"}
		r.Logger.Bodyf("Compiling contracts")
		if _, err := r.Execute(PlanEntrySui, args); err != nil {
			return libcnb.Layer{}, fmt.Errorf("unable to compile contract\n%w", err)
		}

		// initialize wallet for deploy
		if ok, err := r.InitializeDeployWallet(); !ok {
			return libcnb.Layer{}, fmt.Errorf("unable to initialize deploy wallet\n%w", err)
		}

		layer.LaunchEnvironment.Append("PATH", ":", bin)
		layer.LaunchEnvironment.Default("MOVE_HOME", moveHome)
		layer.LaunchEnvironment.Default("SUI_CONFIG_DIR", suiConfig)
		return layer, nil
	})
}

func (r Sui) Name() string {
	return r.LayerContributor.LayerName()
}

func (r Sui) Execute(command string, args []string) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	if err := r.Executor.Execute(effect.Execution{
		Command: command,
		Args:    args,
		Stdout:  buf,
		Stderr:  buf,
	}); err != nil {
		return buf, fmt.Errorf("%s: %w", buf.String(), err)
	}
	return buf, nil
}

func (r Sui) BuildProcessTypes(cr libpak.ConfigurationResolver, app libcnb.Application) ([]libcnb.Process, error) {
	processes := []libcnb.Process{}

	enableDeploy := cr.ResolveBool("BP_ENABLE_SUI_DEPLOY")
	if enableDeploy {
		deployPrivateKey, _ := r.configResolver.Resolve("BP_SUI_DEPLOY_PRIVATE_KEY")
		if len(deployPrivateKey) == 0 {
			return processes, fmt.Errorf("BP_SUI_DEPLOY_PRIVATE_KEY must be specified")
		}

		gasBudget, _ := cr.Resolve("BP_SUI_DEPLOY_GAS")
		processes = append(processes, libcnb.Process{
			Type:      PlanEntrySui,
			Command:   PlanEntrySui,
			Arguments: []string{"client", "publish", "--skip-fetch-latest-git-deps", "--gas-budget", gasBudget},
			Default:   true,
		})
	}
	return processes, nil
}

func (r Sui) InitializeDeployWallet() (bool, error) {
	enableDeploy := r.configResolver.ResolveBool("BP_ENABLE_SUI_DEPLOY")
	if enableDeploy {
		deployPrivateKey, _ := r.configResolver.Resolve("BP_SUI_DEPLOY_PRIVATE_KEY")
		deployKeyScheme, _ := r.configResolver.Resolve("BP_SUI_DEPLOY_KEY_SCHEME")
		deployNetwork, _ := r.configResolver.Resolve("BP_SUI_DEPLOY_NETWORK")
		ok, err := r.InitializeWallet(deployPrivateKey, deployKeyScheme, deployNetwork)
		if !ok {
			return false, fmt.Errorf("unable to initialize %s wallet\n%w", PlanEntrySui, err)
		}
	}
	return true, nil
}

func (r Sui) InitializeWallet(deployPrivateKey, deployKeyScheme, deployNetwork string) (bool, error) {
	if _, err := r.InitializeEnv(); err != nil {
		return false, fmt.Errorf("unable to initialize sui env\n%w", err)
	}

	buf, err := r.ImportingDeployKey(deployPrivateKey, deployKeyScheme)
	if err != nil {
		return false, fmt.Errorf("unable to import sui deploy key\n%w", err)
	}

	deployWallet, err := r.VerifyingDeployKey(buf.Bytes(), deployPrivateKey, deployKeyScheme)
	if err != nil {
		return false, fmt.Errorf("unable to verify sui deploy key\n%w", err)
	}

	if _, err := r.SwitchDeployWallet(deployWallet, deployNetwork); err != nil {
		return false, fmt.Errorf("unable to switch sui deploy wallet\n%w", err)
	}

	if _, err := r.GetFaucet(deployWallet.Address, deployNetwork); err != nil {
		return false, fmt.Errorf("unable to get sui faucet\n%w", err)
	}
	return true, nil
}

func (r Sui) InitializeEnv() (*bytes.Buffer, error) {
	r.Logger.Bodyf("Initializing sui env")
	args := []string{
		"client",
		"--yes",
		"envs",
	}
	return r.Execute(PlanEntrySui, args)
}

func (r Sui) ImportingDeployKey(deployPrivateKey, deployKeyScheme string) (*bytes.Buffer, error) {
	r.Logger.Bodyf("Importing sui deploy key %s", deployPrivateKey)
	args := []string{
		"keytool",
		"import",
		"--json",
		deployPrivateKey,
		deployKeyScheme,
	}
	return r.Execute(PlanEntrySui, args)
}

func (r Sui) VerifyingDeployKey(privateKeyData []byte, deployPrivateKey, deployKeyScheme string) (DeployWallet, error) {
	r.Logger.Bodyf("Verifying sui deploy key %s", deployPrivateKey)
	deployWallet := DeployWallet{}
	if err := json.Unmarshal(privateKeyData, &deployWallet); err != nil {
		return deployWallet, fmt.Errorf("unable to parse sui deploy key\n%w", err)
	}

	if deployWallet.KeyScheme != deployKeyScheme {
		return deployWallet, fmt.Errorf("unable to verify sui deploy key %s", deployPrivateKey)
	}
	return deployWallet, nil
}

func (r Sui) SwitchDeployWallet(deployWallet DeployWallet, deployNetwork string) (bool, error) {
	r.Logger.Bodyf("Switching sui deploy wallet %s for %s as default", deployWallet.Address, deployNetwork)
	args := []string{
		"client",
		"switch",
		"--address", deployWallet.Address,
		"--env", deployNetwork,
	}
	if _, err := r.Execute(PlanEntrySui, args); err != nil {
		return false, fmt.Errorf("unable to switch sui deploy wallet as default\n%w", err)
	}
	return true, nil
}

// Refer to: https://docs.sui.io/guides/developer/getting-started/get-coins#request-test-tokens-through-curl
//
//	curl --location --request POST 'https://faucet.devnet.sui.io/gas' \
//		--header 'Content-Type: application/json' \
//		--data-raw "{
//			\"FixedAmountRequest\": {
//				\"recipient\": \"${recipient}\"
//			}
//		}"
func (r Sui) GetFaucet(recipient, deployNetwork string) (bool, error) {
	if deployNetwork == "devnet" {
		r.Logger.Bodyf("Getting sui faucet for %s", recipient)
		args := []string{
			"--location",
			"--request", "POST", fmt.Sprintf("https://faucet.%s.sui.io/gas", deployNetwork),
			"--header", "Content-Type: application/json",
			"--data-raw", fmt.Sprintf("{\"FixedAmountRequest\": {\"recipient\": \"%s\"}}", recipient),
		}
		if _, err := r.Execute("curl", args); err != nil {
			return false, fmt.Errorf("unable to get sui faucet for %s\n%w", recipient, err)
		}
	}
	return true, nil
}
