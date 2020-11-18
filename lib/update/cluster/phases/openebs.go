/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package phases

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"github.com/gravitational/gravity/lib/app/hooks"
	"github.com/gravitational/gravity/lib/fsm"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/utils"
	"github.com/gravitational/gravity/lib/utils/kubectl"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"k8s.io/client-go/kubernetes"
)

// Upgrade OpenEBS
// Following the steps from the OpenEBS' web site;
// https://github.com/openebs/openebs/blob/master/k8s/upgrades/README.md

// PhaseUpgradePool backs up etcd data on all servers
type PhaseUpgradePool struct {
	// FieldLogger is used for logging
	log.FieldLogger
	// Client is an API client to the kubernetes API
	Client          *kubernetes.Clientset
	Pool            string
	PoolFromVersion string
	PoolToVersion   string
}

func NewPhaseUpgradePool(phase storage.OperationPhase, client *kubernetes.Clientset, logger log.FieldLogger) (fsm.PhaseExecutor, error) {

	poolAndVer := strings.Split(phase.Data.Data, " ")
	return &PhaseUpgradePool{
		FieldLogger:     logger,
		Client:          client,
		Pool:            poolAndVer[0],
		PoolFromVersion: poolAndVer[1],
		PoolToVersion:   poolAndVer[2],
	}, nil
}

func (p *PhaseUpgradePool) Execute(ctx context.Context) error {
	p.Info("Upgrading OpenEBS poolsAndVersion.")

	err := p.execPoolUpgradeCmd(ctx, p.Pool, p.PoolFromVersion)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

type PoolUpgrade struct {
	FromVersion string
	Pool        string
}

func (p *PhaseUpgradePool) execPoolUpgradeCmd(ctx context.Context, pool string, version string) error {
	out, err := execUpgradeJob(ctx, poolUpgradeTemplate, &PoolUpgrade{FromVersion: version, Pool: pool}, "cstor-spc-1170220", p.Client)
	if out != "" {
		p.Infof("OpenEBS pool upgrade job output: %v", out)
	}

	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func (p *PhaseUpgradePool) Rollback(context.Context) error {
	return nil
}

func (*PhaseUpgradePool) PreCheck(ctx context.Context) error {
	return nil
}

func (*PhaseUpgradePool) PostCheck(context.Context) error {
	return nil
}

// The upgrade jobs are taken from the following OpenEBS's upgrade procedure:
// https://github.com/openebs/openebs/blob/master/k8s/upgrades/README.md
var poolUpgradeTemplate = template.Must(template.New("upgradePool").Parse(`
#This is an example YAML for upgrading cstor SPC.
#Some of the values below needs to be changed to
#match your openebs installation. The fields are
#indicated with VERIFY
---
apiVersion: batch/v1
kind: Job
metadata:
  #VERIFY that you have provided a unique name for this upgrade job.
  #The name can be any valid K8s string for name. This example uses
  #the following convention: cstor-spc-<flattened-from-to-versions>
  name: cstor-spc-1170220

  #VERIFY the value of namespace is same as the namespace where openebs components
  # are installed. You can verify using the command:
  # kubectl get pods -n <openebs-namespace> -l openebs.io/component-name=maya-apiserver
  # The above command should return status of the openebs-apiserver.
  namespace: openebs
spec:
  template:
    spec:
      #VERIFY the value of serviceAccountName is pointing to service account
      # created within openebs namespace. Use the non-default account.
      # by running kubectl get sa -n <openebs-namespace>
      serviceAccountName: openebs-maya-operator
      containers:
      - name:  upgrade
        args:
        - "cstor-spc"

        # --from-version is the current version of the pool
        - "--from-version={{.FromVersion}}"

        # --to-version is the version desired upgrade version
        - "--to-version=2.2.0"

        # Bulk upgrade is supported
        # To make use of it, please provide the list of SPCs
        # as mentioned below
        - "{{.Pool}}"

        #Following are optional parameters
        #Log Level
        - "--v=4"
        #DO NOT CHANGE BELOW PARAMETERS
        env:
        - name: OPENEBS_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        tty: true

        # the image version should be same as the --to-version mentioned above
        # in the args of the job
        image: openebs/m-upgrade:2.2.0
        imagePullPolicy: Always
      restartPolicy: Never
---
`))

// PhaseUpgradeVolumes backs up etcd data on all servers
type PhaseUpgradeVolumes struct {
	// FieldLogger is used for logging
	log.FieldLogger
	// Client is an API client to the kubernetes API
	Client            *kubernetes.Clientset
	Volume            string
	VolumeFromVersion string
	VolumeToVersion   string
}

func NewPhaseUpgradeVolume(phase storage.OperationPhase, client *kubernetes.Clientset, logger log.FieldLogger) (fsm.PhaseExecutor, error) {

	volAndVer := strings.Split(phase.Data.Data, " ")

	return &PhaseUpgradeVolumes{
		FieldLogger:       logger,
		Client:            client,
		Volume:            volAndVer[0],
		VolumeFromVersion: volAndVer[1],
		VolumeToVersion:   volAndVer[2],
	}, nil
}

func (p *PhaseUpgradeVolumes) Execute(ctx context.Context) error {
	p.Info("Upgrading OpenEBS volumes.")

	err := p.execVolumeUpgradeCmd(ctx, p.Volume, p.VolumeFromVersion)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

type VolumeUpgrade struct {
	FromVersion string
	Volume      string
}

func (p *PhaseUpgradeVolumes) execVolumeUpgradeCmd(ctx context.Context, volume string, fromVersion string) error {
	out, err := execUpgradeJob(ctx, volumeUpgradeTemplate, &VolumeUpgrade{FromVersion: fromVersion, Volume: volume}, "cstor-vol-170220", p.Client)
	if out != "" {
		p.Infof("OpenEBS volume upgrade job output: %v", out)
	}

	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func execUpgradeJob(ctx context.Context, template *template.Template, templateData interface{}, jobName string, client *kubernetes.Clientset) (string, error) {
	var buf bytes.Buffer
	err := template.Execute(&buf, templateData)
	if err != nil {
		return "", trace.Wrap(err)
	}

	upgradeJobFile := "openebs_data_plane_component_upgrade.yaml"
	err = ioutil.WriteFile(upgradeJobFile, buf.Bytes(), 0644)
	if err != nil {
		return "", trace.Wrap(err)
	}

	//var out bytes.Buffer
	//var kubectlJobOut bytes.Buffer
	//if err := utils.Exec(exec.Command("/bin/bash", "-c", fmt.Sprintf("kubectl apply -f %v", upgradeJobFile)), &kubectlJobOut); err != nil {
	kubectlJobOut, err := kubectl.Apply(upgradeJobFile)
	if err != nil {
		return fmt.Sprintf("Failed to upgrade openEBS data plane component. Output %v:", string(kubectlJobOut)), trace.Wrap(err)
	}

	/*
		if err := utils.Exec(exec.Command("/bin/bash", "-c", fmt.Sprintf("kubectl apply -f %v", upgradeJobFile)), &kubectlJobOut); err != nil {
			out.WriteString(fmt.Sprintf("Failed volume upgrade k8s exec command. Got output %v:", kubectlJobOut.String()))
			return out.String(), trace.Wrap(err)
		}

	*/

	runner, err := hooks.NewRunner(client)
	if err != nil {
		return "", trace.Wrap(err)
	}

	namespace := "openebs"
	err = runner.Wait(ctx, hooks.JobRef{Name: jobName, Namespace: namespace})
	if err != nil {
		return "", trace.Wrap(err)
	}

	upgradeJobLog := utils.NewSyncBuffer()
	err = runner.StreamLogs(ctx, hooks.JobRef{Name: jobName, Namespace: namespace}, upgradeJobLog)
	if err != nil {
		return "", trace.Wrap(err)
	}

	return upgradeJobLog.String(), nil
}

var volumeUpgradeTemplate = template.Must(template.New("upgradeVolumes").Parse(`
#This is an example YAML for upgrading cstor volume.
#Some of the values below needs to be changed to
#match your openebs installation. The fields are
#indicated with VERIFY
---
apiVersion: batch/v1
kind: Job
metadata:
  #VERIFY that you have provided a unique name for this upgrade job.
  #The name can be any valid K8s string for name. This example uses
  #the following convention: cstor-vol-<flattened-from-to-versions>
  name: cstor-vol-170220

  #VERIFY the value of namespace is same as the namespace
  # where openebs components
  # are installed. You can verify using the command:
  # kubectl get pods -n <openebs-namespace> -l
  # openebs.io/component-name=maya-apiserver
  # The above command should return status of the openebs-apiserver.
  namespace: openebs


spec:
  template:
    spec:
      #VERIFY the value of serviceAccountName is pointing to service account
      # created within openebs namespace. Use the non-default account.
      # by running kubectl get sa -n <openebs-namespace>
      serviceAccountName: openebs-maya-operator
      containers:
        - name: upgrade
          args:
            - "cstor-volume"

            # --from-version is the current version of the volume
            - "--from-version={{.FromVersion}}"

            # --to-version is the version desired upgrade version
            - "--to-version=2.2.0"

            # Bulk upgrade is supported from 1.9
            # To make use of it, please provide the list of PVs
            # as mentioned below
            - "{{.Volume}}"

            #Following are optional parameters
            #Log Level
            - "--v=4"
          #DO NOT CHANGE BELOW PARAMETERS
          env:
            - name: OPENEBS_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          tty: true

          # the image version should be same as the --to-version mentioned above
          # in the args of the job
          image: quay.io/openebs/m-upgrade:2.2.0
          imagePullPolicy: Always
      restartPolicy: Never
---
`))

func (p *PhaseUpgradeVolumes) Rollback(context.Context) error {
	return nil
}

func (*PhaseUpgradeVolumes) PreCheck(ctx context.Context) error {
	return nil
}

func (*PhaseUpgradeVolumes) PostCheck(context.Context) error {
	return nil
}
