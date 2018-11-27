/*
Copyright 2018 The Kubernetes Authors.

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

package machine

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	errorutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"

	providerconfigv1 "sigs.k8s.io/cluster-api-provider-aws/pkg/apis/awsproviderconfig/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clustererror "sigs.k8s.io/cluster-api/pkg/controller/error"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/elbv2"

	"k8s.io/apimachinery/pkg/runtime"
	awsclient "sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/client"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	userDataSecretKey         = "userData"
	ec2InstanceIDNotFoundCode = "InvalidInstanceID.NotFound"
	requeueAfterSeconds       = 20

	// MachineCreationSucceeded indicates success for machine creation
	MachineCreationSucceeded = "MachineCreationSucceeded"

	// MachineCreationFailed indicates that machine creation failed
	MachineCreationFailed = "MachineCreationFailed"
)

// MachineActuator is a variable used to include the actuator into the machine controller
var MachineActuator *Actuator

// Actuator is the AWS-specific actuator for the Cluster API machine controller
type Actuator struct {
	kubeClient       kubernetes.Interface
	client           client.Client
	awsClientBuilder awsclient.AwsClientBuilderFuncType
	codec            codec
}

// ActuatorParams holds parameter information for Actuator
type ActuatorParams struct {
	KubeClient       kubernetes.Interface
	Client           client.Client
	AwsClientBuilder awsclient.AwsClientBuilderFuncType
	Codec            codec
}

type codec interface {
	DecodeProviderConfig(*clusterv1.ProviderConfig, runtime.Object) error
	DecodeProviderStatus(*runtime.RawExtension, runtime.Object) error
	EncodeProviderStatus(runtime.Object) (*runtime.RawExtension, error)
}

// NewActuator returns a new AWS Actuator
func NewActuator(params ActuatorParams) (*Actuator, error) {
	actuator := &Actuator{
		kubeClient:       params.KubeClient,
		client:           params.Client,
		awsClientBuilder: params.AwsClientBuilder,
		codec:            params.Codec,
	}
	return actuator, nil
}

// Create runs a new EC2 instance
func (a *Actuator) Create(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	glog.Info("creating machine")
	instance, err := a.CreateMachine(cluster, machine)
	if err != nil {
		glog.Errorf("error creating machine: %v", err)
		updateConditionError := a.updateMachineProviderConditions(machine, providerconfigv1.MachineCreation, MachineCreationFailed, err.Error())
		if updateConditionError != nil {
			glog.Errorf("error updating machine conditions: %v", updateConditionError)
		}
		return err
	}
	return a.updateStatus(machine, instance)
}

func (a *Actuator) updateMachineStatus(machine *clusterv1.Machine, awsStatus *providerconfigv1.AWSMachineProviderStatus, networkAddresses []corev1.NodeAddress) error {
	awsStatusRaw, err := EncodeProviderStatus(a.codec, awsStatus)
	if err != nil {
		glog.Errorf("error encoding AWS provider status: %v", err)
		return err
	}

	machineCopy := machine.DeepCopy()
	machineCopy.Status.ProviderStatus = awsStatusRaw
	if networkAddresses != nil {
		machineCopy.Status.Addresses = networkAddresses
	}
	oldAWSStatus, err := ProviderStatusFromMachine(a.codec, machine)
	if err != nil {
		glog.Errorf("error updating machine status: %v", err)
		return err
	}
	// TODO(vikasc): Revisit to compare complete machine status objects
	if !equality.Semantic.DeepEqual(awsStatus, oldAWSStatus) || !equality.Semantic.DeepEqual(machine.Status.Addresses, machineCopy.Status.Addresses) {
		glog.Infof("machine status has changed, updating")
		time := metav1.Now()
		machineCopy.Status.LastUpdated = &time

		if err := a.client.Status().Update(context.Background(), machineCopy); err != nil {
			glog.Errorf("error updating machine status: %v", err)
			return err
		}
	} else {
		glog.Info("status unchanged")
	}

	return nil
}

// updateMachineProviderConditions updates conditions set within machine provider status.
func (a *Actuator) updateMachineProviderConditions(machine *clusterv1.Machine, conditionType providerconfigv1.AWSMachineProviderConditionType, reason string, msg string) error {

	glog.Info("updating machine conditions")

	awsStatus, err := ProviderStatusFromMachine(a.codec, machine)
	if err != nil {
		glog.Errorf("error decoding machine provider status: %v", err)
		return err
	}

	awsStatus.Conditions = SetAWSMachineProviderCondition(awsStatus.Conditions, conditionType, corev1.ConditionTrue, reason, msg, UpdateConditionIfReasonOrMessageChange)

	err = a.updateMachineStatus(machine, awsStatus, nil)
	if err != nil {
		return err
	}

	return nil
}

// CreateMachine starts a new AWS instance as described by the cluster and machine resources
func (a *Actuator) CreateMachine(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (*ec2.Instance, error) {
	machineProviderConfig, err := ProviderConfigFromMachine(machine)
	if err != nil {
		glog.Errorf("error decoding MachineProviderConfig: %v", err)
		return nil, err
	}

	credentialsSecretName := ""
	if machineProviderConfig.CredentialsSecret != nil {
		credentialsSecretName = machineProviderConfig.CredentialsSecret.Name
	}
	client, err := a.awsClientBuilder(a.kubeClient, credentialsSecretName, machine.Namespace, machineProviderConfig.Placement.Region)
	if err != nil {
		glog.Errorf("unable to obtain AWS client: %v", err)
		return nil, fmt.Errorf("unable to obtain AWS client: %v", err)
	}

	// We explicitly do NOT want to remove stopped masters.
	if !IsMaster(machine) {
		// Prevent having a lot of stopped nodes sitting around.
		err = removeStoppedMachine(machine, client)
		if err != nil {
			glog.Errorf("unable to remove stopped machines: %v", err)
			return nil, fmt.Errorf("unable to remove stopped nodes: %v", err)
		}
	}

	userData := []byte{}
	if machineProviderConfig.UserDataSecret != nil {
		userDataSecret, err := a.kubeClient.CoreV1().Secrets(machine.Namespace).Get(machineProviderConfig.UserDataSecret.Name, metav1.GetOptions{})
		if err != nil {
			glog.Errorf("error getting user data secret %s: %v", machineProviderConfig.UserDataSecret.Name, err)
			return nil, err
		}
		if data, exists := userDataSecret.Data[userDataSecretKey]; exists {
			userData = data
		} else {
			glog.Warningf("Secret %v/%v does not have %q field set. Thus, no user data applied when creating an instance.", machine.Namespace, machineProviderConfig.UserDataSecret.Name, userDataSecretKey)
		}
	}

	instance, err := launchInstance(machine, machineProviderConfig, userData, client)
	if err != nil {
		retErr := fmt.Errorf("error launching instance: %v", err)
		glog.Error(retErr)
		return nil, retErr
	}

	err = a.UpdateLoadBalancers(client, machineProviderConfig, instance)

	return instance, err
}

// Delete deletes a machine and updates its finalizer
func (a *Actuator) Delete(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	glog.Info("deleting machine")
	if err := a.DeleteMachine(cluster, machine); err != nil {
		glog.Errorf("error deleting machine: %v", err)
		return err
	}
	return nil
}

// DeleteMachine deletes an AWS instance
func (a *Actuator) DeleteMachine(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	machineProviderConfig, err := ProviderConfigFromMachine(machine)
	if err != nil {
		glog.Errorf("error decoding MachineProviderConfig: %v", err)
		return err
	}

	region := machineProviderConfig.Placement.Region
	credentialsSecretName := ""
	if machineProviderConfig.CredentialsSecret != nil {
		credentialsSecretName = machineProviderConfig.CredentialsSecret.Name
	}
	client, err := a.awsClientBuilder(a.kubeClient, credentialsSecretName, machine.Namespace, region)
	if err != nil {
		glog.Errorf("error getting EC2 client: %v", err)
		return fmt.Errorf("error getting EC2 client: %v", err)
	}

	instances, err := GetRunningInstances(machine, client)
	if err != nil {
		glog.Errorf("error getting running instances: %v", err)
		return err
	}
	if len(instances) == 0 {
		glog.Warningf("no instances found to delete for machine")
		return nil
	}

	return TerminateInstances(client, instances)
}

// Update attempts to sync machine state with an existing instance. Today this just updates status
// for details that may have changed. (IPs and hostnames) We do not currently support making any
// changes to actual machines in AWS. Instead these will be replaced via MachineDeployments.
func (a *Actuator) Update(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	glog.Info("updating machine")

	machineProviderConfig, err := ProviderConfigFromMachine(machine)
	if err != nil {
		glog.Errorf("error decoding MachineProviderConfig: %v", err)
		return err
	}

	region := machineProviderConfig.Placement.Region
	glog.Info("obtaining EC2 client for region")
	credentialsSecretName := ""
	if machineProviderConfig.CredentialsSecret != nil {
		credentialsSecretName = machineProviderConfig.CredentialsSecret.Name
	}
	client, err := a.awsClientBuilder(a.kubeClient, credentialsSecretName, machine.Namespace, region)
	if err != nil {
		glog.Errorf("error getting EC2 client: %v", err)
		return fmt.Errorf("unable to obtain EC2 client: %v", err)
	}

	instances, err := GetRunningInstances(machine, client)
	if err != nil {
		glog.Errorf("error getting running instances: %v", err)
		return err
	}
	glog.Infof("found %d instances for machine", len(instances))

	// Parent controller should prevent this from ever happening by calling Exists and then Create,
	// but instance could be deleted between the two calls.
	if len(instances) == 0 {
		glog.Warningf("attempted to update machine but no instances found")
		// Update status to clear out machine details.
		err := a.updateStatus(machine, nil)
		if err != nil {
			return err
		}
		glog.Errorf("attempted to update machine but no instances found")
		return fmt.Errorf("attempted to update machine but no instances found")
	}

	glog.Info("instance found")

	// In very unusual circumstances, there could be more than one machine running matching this
	// machine name and cluster ID. In this scenario we will keep the newest, and delete all others.
	sortInstances(instances)
	if len(instances) > 1 {
		err = TerminateInstances(client, instances[1:])
		if err != nil {
			return err
		}
	}

	newestInstance := instances[0]

	err = a.UpdateLoadBalancers(client, machineProviderConfig, newestInstance)
	if err != nil {
		glog.Errorf("error updating load balancers: %v", err)
		return err
	}

	// We do not support making changes to pre-existing instances, just update status.
	return a.updateStatus(machine, newestInstance)
}

// Exists determines if the given machine currently exists. For AWS we query for instances in
// running state, with a matching name tag, to determine a match.
func (a *Actuator) Exists(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (bool, error) {
	glog.Info("checking if machine exists")

	instances, err := a.getMachineInstances(cluster, machine)
	if err != nil {
		glog.Errorf("error getting running instances: %v", err)
		return false, err
	}
	if len(instances) == 0 {
		glog.Info("instance does not exist")
		return false, nil
	}

	// If more than one result was returned, it will be handled in Update.
	glog.Infof("instance exists as %q", *instances[0].InstanceId)
	return true, nil
}

// Describe provides information about machine's instance(s)
func (a *Actuator) Describe(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (*ec2.Instance, error) {
	glog.Infof("checking if machine exists")

	instances, err := a.getMachineInstances(cluster, machine)
	if err != nil {
		glog.Errorf("error getting running instances: %v", err)
		return nil, err
	}
	if len(instances) == 0 {
		glog.Info("instance does not exist")
		return nil, nil
	}

	return instances[0], nil
}

func (a *Actuator) getMachineInstances(cluster *clusterv1.Cluster, machine *clusterv1.Machine) ([]*ec2.Instance, error) {
	machineProviderConfig, err := ProviderConfigFromMachine(machine)
	if err != nil {
		glog.Errorf("error decoding MachineProviderConfig: %v", err)
		return nil, err
	}

	region := machineProviderConfig.Placement.Region
	credentialsSecretName := ""
	if machineProviderConfig.CredentialsSecret != nil {
		credentialsSecretName = machineProviderConfig.CredentialsSecret.Name
	}
	client, err := a.awsClientBuilder(a.kubeClient, credentialsSecretName, machine.Namespace, region)
	if err != nil {
		glog.Errorf("error getting EC2 client: %v", err)
		return nil, fmt.Errorf("error getting EC2 client: %v", err)
	}

	return GetRunningInstances(machine, client)
}

// UpdateLoadBalancers adds a given machine instance to the load balancers specified in its provider config
func (a *Actuator) UpdateLoadBalancers(client awsclient.Client, providerConfig *providerconfigv1.AWSMachineProviderConfig, instance *ec2.Instance) error {
	if len(providerConfig.LoadBalancers) == 0 {
		glog.V(4).Infof("Instance %q has no load balancers configured. Skipping", *instance.InstanceId)
		return nil
	}
	errs := []error{}
	classicLoadBalancerNames := []string{}
	networkLoadBalancerNames := []string{}
	for _, loadBalancerRef := range providerConfig.LoadBalancers {
		switch loadBalancerRef.Type {
		case providerconfigv1.NetworkLoadBalancerType:
			networkLoadBalancerNames = append(networkLoadBalancerNames, loadBalancerRef.Name)
		case providerconfigv1.ClassicLoadBalancerType:
			classicLoadBalancerNames = append(classicLoadBalancerNames, loadBalancerRef.Name)
		}
	}

	var err error
	if len(classicLoadBalancerNames) > 0 {
		err := a.registerWithClassicLoadBalancers(client, classicLoadBalancerNames, instance)
		if err != nil {
			glog.Errorf("failed to register classic load balancers: %v", err)
			errs = append(errs, err)
		}
	}
	if len(networkLoadBalancerNames) > 0 {
		err = a.registerWithNetworkLoadBalancers(client, networkLoadBalancerNames, instance)
		if err != nil {
			glog.Errorf("failed to register network load balancers: %v", err)
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errorutil.NewAggregate(errs)
	}
	return nil
}

func (a *Actuator) registerWithClassicLoadBalancers(client awsclient.Client, names []string, instance *ec2.Instance) error {
	glog.V(4).Infof("Updating classic load balancer registration for %q", *instance.InstanceId)
	elbInstance := &elb.Instance{InstanceId: instance.InstanceId}
	var errs []error
	for _, elbName := range names {
		req := &elb.RegisterInstancesWithLoadBalancerInput{
			Instances:        []*elb.Instance{elbInstance},
			LoadBalancerName: aws.String(elbName),
		}
		_, err := client.RegisterInstancesWithLoadBalancer(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", elbName, err))
		}
	}

	if len(errs) > 0 {
		return errorutil.NewAggregate(errs)
	}
	return nil
}

func (a *Actuator) registerWithNetworkLoadBalancers(client awsclient.Client, names []string, instance *ec2.Instance) error {
	glog.V(4).Infof("Updating network load balancer registration for %q", *instance.InstanceId)
	lbNames := make([]*string, len(names))
	for i, name := range names {
		lbNames[i] = aws.String(name)
	}
	lbsRequest := &elbv2.DescribeLoadBalancersInput{
		Names: lbNames,
	}
	lbsResponse, err := client.ELBv2DescribeLoadBalancers(lbsRequest)
	if err != nil {
		glog.Errorf("failed to describe load balancers %v: %v", names, err)
		return err
	}
	// Use a map for target groups to get unique target group entries across load balancers
	targetGroups := map[string]*elbv2.TargetGroup{}
	for _, loadBalancer := range lbsResponse.LoadBalancers {
		glog.V(4).Infof("retrieving target groups for load balancer %q", *loadBalancer.LoadBalancerName)
		targetGroupsInput := &elbv2.DescribeTargetGroupsInput{
			LoadBalancerArn: loadBalancer.LoadBalancerArn,
		}
		targetGroupsOutput, err := client.ELBv2DescribeTargetGroups(targetGroupsInput)
		if err != nil {
			glog.Errorf("failed to retrieve load balancer target groups for %q: %v", *loadBalancer.LoadBalancerName, err)
			return err
		}
		for _, targetGroup := range targetGroupsOutput.TargetGroups {
			targetGroups[*targetGroup.TargetGroupArn] = targetGroup
		}
	}
	if glog.V(4) {
		targetGroupArns := make([]string, 0, len(targetGroups))
		for arn := range targetGroups {
			targetGroupArns = append(targetGroupArns, fmt.Sprintf("%q", arn))
		}
		glog.Infof("registering instance %q with target groups: %v", *instance.InstanceId, strings.Join(targetGroupArns, ","))
	}
	errs := []error{}
	for _, targetGroup := range targetGroups {
		var target *elbv2.TargetDescription
		switch *targetGroup.TargetType {
		case elbv2.TargetTypeEnumInstance:
			target = &elbv2.TargetDescription{
				Id: instance.InstanceId,
			}
		case elbv2.TargetTypeEnumIp:
			target = &elbv2.TargetDescription{
				Id: instance.PrivateIpAddress,
			}
		}
		registerTargetsInput := &elbv2.RegisterTargetsInput{
			TargetGroupArn: targetGroup.TargetGroupArn,
			Targets:        []*elbv2.TargetDescription{target},
		}
		_, err := client.ELBv2RegisterTargets(registerTargetsInput)
		if err != nil {
			glog.Errorf("failed to register instance %q with target group %q: %v", *instance.InstanceId, *targetGroup.TargetGroupArn, err)
			errs = append(errs, fmt.Errorf("%s: %v", *targetGroup.TargetGroupArn, err))
		}
	}
	if len(errs) > 0 {
		return errorutil.NewAggregate(errs)
	}
	return nil
}

// updateStatus calculates the new machine status, checks if anything has changed, and updates if so.
func (a *Actuator) updateStatus(machine *clusterv1.Machine, instance *ec2.Instance) error {

	glog.Info("updating status")

	// Starting with a fresh status as we assume full control of it here.
	awsStatus, err := ProviderStatusFromMachine(a.codec, machine)
	if err != nil {
		glog.Errorf("error decoding machine provider status: %v", err)
		return err
	}
	// Save this, we need to check if it changed later.
	networkAddresses := []corev1.NodeAddress{}

	// Instance may have existed but been deleted outside our control, clear it's status if so:
	if instance == nil {
		awsStatus.InstanceID = nil
		awsStatus.InstanceState = nil
	} else {
		awsStatus.InstanceID = instance.InstanceId
		awsStatus.InstanceState = instance.State.Name
		if instance.PublicIpAddress != nil {
			networkAddresses = append(networkAddresses, corev1.NodeAddress{
				Type:    corev1.NodeExternalIP,
				Address: *instance.PublicIpAddress,
			})
		}
		if instance.PrivateIpAddress != nil {
			networkAddresses = append(networkAddresses, corev1.NodeAddress{
				Type:    corev1.NodeInternalIP,
				Address: *instance.PrivateIpAddress,
			})
		}
		if instance.PublicDnsName != nil {
			networkAddresses = append(networkAddresses, corev1.NodeAddress{
				Type:    corev1.NodeExternalDNS,
				Address: *instance.PublicDnsName,
			})
		}
		if instance.PrivateDnsName != nil {
			networkAddresses = append(networkAddresses, corev1.NodeAddress{
				Type:    corev1.NodeInternalDNS,
				Address: *instance.PrivateDnsName,
			})
		}
	}
	glog.Info("finished calculating AWS status")

	awsStatus.Conditions = SetAWSMachineProviderCondition(awsStatus.Conditions, providerconfigv1.MachineCreation, corev1.ConditionTrue, MachineCreationSucceeded, "machine successfully created", UpdateConditionIfReasonOrMessageChange)
	// TODO(jchaloup): do we really need to update tis?
	// origInstanceID := awsStatus.InstanceID
	// if !StringPtrsEqual(origInstanceID, awsStatus.InstanceID) {
	// 	mLog.Debug("AWS instance ID changed, clearing LastELBSync to trigger adding to ELBs")
	// 	awsStatus.LastELBSync = nil
	// }

	err = a.updateMachineStatus(machine, awsStatus, networkAddresses)
	if err != nil {
		return err
	}

	// If machine state is still pending, we will return an error to keep the controllers
	// attempting to update status until it hits a more permanent state. This will ensure
	// we get a public IP populated more quickly.
	if awsStatus.InstanceState != nil && *awsStatus.InstanceState == ec2.InstanceStateNamePending {
		glog.Infof("instance state still pending, returning an error to requeue")
		return &clustererror.RequeueAfterError{RequeueAfter: requeueAfterSeconds * time.Second}
	}
	return nil
}

func getClusterID(machine *clusterv1.Machine) (string, bool) {
	clusterID, ok := machine.Labels[providerconfigv1.ClusterIDLabel]
	return clusterID, ok
}
