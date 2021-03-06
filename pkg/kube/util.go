package kube

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmddapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/supergiant/control/pkg/clouds"
	"github.com/supergiant/control/pkg/model"
	"github.com/supergiant/control/pkg/sgerrors"
	"github.com/supergiant/control/pkg/util"
	"github.com/supergiant/control/pkg/workflows/steps"
	"github.com/supergiant/control/pkg/workflows/steps/amazon"
)

func processAWSMetrics(k *model.Kube, metrics map[string]map[string]interface{}) {
	for _, masterNode := range k.Masters {
		// After some amount of time prometheus start using region in metric name
		prefix := ip2Host(masterNode.PrivateIp)
		for metricKey := range metrics {
			if strings.Contains(metricKey, prefix) {
				value := metrics[metricKey]
				delete(metrics, metricKey)
				metrics[strings.ToLower(masterNode.Name)] = value
			}
		}
	}

	for _, workerNode := range k.Nodes {
		prefix := ip2Host(workerNode.PrivateIp)

		for metricKey := range metrics {
			if strings.Contains(metricKey, prefix) {
				value := metrics[metricKey]
				delete(metrics, metricKey)
				metrics[strings.ToLower(workerNode.Name)] = value
			}
		}
	}
}

func ip2Host(ip string) string {
	return fmt.Sprintf("ip-%s", strings.Join(strings.Split(ip, "."), "-"))
}

func kubeFromKubeConfig(kubeConfig clientcmddapi.Config) (*model.Kube, error) {
	currentCtxName := kubeConfig.CurrentContext
	currentContext := kubeConfig.Contexts[currentCtxName]

	if currentContext == nil {
		return nil, errors.Wrapf(sgerrors.ErrNilEntity, "current context %s not found in context map %v",
			currentCtxName, kubeConfig.Contexts)
	}

	authInfoName := currentContext.AuthInfo
	authInfo := kubeConfig.AuthInfos[authInfoName]

	if authInfo == nil {
		return nil, errors.Wrapf(sgerrors.ErrNilEntity, "authInfo %s not found in auth into auth map %v",
			authInfoName, kubeConfig.AuthInfos)
	}

	clusterName := currentContext.Cluster
	cluster := kubeConfig.Clusters[clusterName]

	if cluster == nil {
		return nil, errors.Wrapf(sgerrors.ErrNilEntity, "cluster %s not found in cluster map %v",
			clusterName, kubeConfig.Clusters)
	}

	return &model.Kube{
		Name:            currentContext.Cluster,
		ExternalDNSName: cluster.Server,
		Auth: model.Auth{
			CACert:    string(cluster.CertificateAuthorityData),
			AdminCert: string(authInfo.ClientCertificateData),
			AdminKey:  string(authInfo.ClientKeyData),
		},
	}, nil
}

func syncMachines(ctx context.Context, k *model.Kube, account *model.CloudAccount) error {
	config := &steps.Config{}
	if err := util.FillCloudAccountCredentials(account, config); err != nil {
		return errors.Wrap(err, "error fill cloud account credentials")
	}

	config.AWSConfig.Region = k.Region
	EC2, err := amazon.GetEC2(config.AWSConfig)

	if err != nil {
		return errors.Wrap(sgerrors.ErrInvalidCredentials, err.Error())
	}

	describeInstanceOutput, err := EC2.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String(fmt.Sprintf("tag:%s", clouds.TagClusterID)),
				Values: aws.StringSlice([]string{k.ID}),
			},
		},
	})

	if err != nil {
		return errors.Wrap(err, "describe instances")
	}

	for _, res := range describeInstanceOutput.Reservations {
		for _, instance := range res.Instances {
			node := &model.Machine{
				Size:   *instance.InstanceType,
				State:  model.MachineStateActive,
				Role:   model.RoleNode,
				Region: k.Region,
			}

			if instance.PublicIpAddress != nil {
				node.PublicIp = *instance.PublicIpAddress
			}

			if instance.PrivateIpAddress != nil {
				node.PrivateIp = *instance.PrivateIpAddress
			}

			for _, tag := range instance.Tags {
				if tag.Key != nil && *tag.Key == clouds.TagNodeName {
					node.Name = *tag.Value
				}
			}

			isFound := false

			for _, machine := range k.Nodes {
				if instance.PrivateIpAddress != nil && machine.PrivateIp == *instance.PrivateIpAddress {
					isFound = true
				}
			}

			var state int64

			if instance.State != nil && instance.State.Code != nil {
				state = *instance.State.Code
			}

			// If node is new in workers and it is not a master
			if !isFound && k.Masters[node.Name] == nil && state == 16 {
				logrus.Debugf("Add new node %v", node)
				k.Nodes[node.Name] = node
			}
		}
	}

	return nil
}

func createSpotInstance(req *SpotRequest, config *steps.Config) error {
	switch config.Provider {
	case clouds.AWS:
		return createAwsSpotInstance(req, config)
	}

	return sgerrors.ErrUnsupportedProvider
}

func getSpotPrices(machineType string, config *steps.Config) ([]string, error) {
	switch config.Provider {
	case clouds.AWS:
		return getAwsSpotPrices(machineType, config)
	}

	return nil, sgerrors.ErrUnsupportedProvider
}

func createAwsSpotInstance(req *SpotRequest, config *steps.Config) error {
	svc, err := amazon.GetEC2(config.AWSConfig)

	if err != nil {
		return errors.Wrap(err, "get EC2 client")
	}

	config.AWSConfig.InstanceType = req.MachineType
	volumeSize, err := strconv.ParseInt(config.AWSConfig.VolumeSize, 10, 64)

	if err != nil {
		return errors.Wrapf(err, "parse volume size %s", config.AWSConfig.VolumeSize)
	}

	input := &ec2.RequestSpotInstancesInput{
		Type: aws.String("persistent"),
		LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
			IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
				Name: aws.String(config.AWSConfig.NodesInstanceProfile),
			},
			SubnetId:         aws.String(config.AWSConfig.Subnets[req.AvailabilityZone]),
			SecurityGroupIds: []*string{aws.String(config.AWSConfig.NodesSecurityGroupID)},
			ImageId:          aws.String(config.AWSConfig.ImageID),
			InstanceType:     aws.String(config.AWSConfig.InstanceType),
			KeyName:          aws.String(config.AWSConfig.KeyPairName),
			BlockDeviceMappings: []*ec2.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/sda1"),
					Ebs: &ec2.EbsBlockDevice{
						DeleteOnTermination: aws.Bool(false),
						VolumeType:          aws.String("gp2"),
						VolumeSize:          aws.Int64(volumeSize),
					},
				},
			},
			UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(
				fmt.Sprintf("#!/bin/sh\n%s", config.ConfigMap.Data)))),
		},
		SpotPrice:     aws.String(req.SpotPrice),
		ClientToken:   aws.String(uuid.New()),
		InstanceCount: aws.Int64(req.MachineCount),
		DryRun:        aws.Bool(config.DryRun),
		ValidFrom:     aws.Time(time.Now().Add(time.Second * 10)),
		// TODO(stgleb): pass this as a parameter
		ValidUntil: aws.Time(time.Now().Add(time.Duration(24*365) * time.Hour)),
	}

	result, err := svc.RequestSpotInstances(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			logrus.Errorf("request spot instance caused %s", aerr.Message())
		} else {
			logrus.Errorf("Error %v", err)
		}
		return errors.Wrap(err, "request spot instance")
	}

	go func() {
		requestIds := make([]*string, 0)

		for _, spot := range result.SpotInstanceRequests {
			requestIds = append(requestIds, spot.SpotInstanceRequestId)
		}

		describeReq := &ec2.DescribeSpotInstanceRequestsInput{
			DryRun:                 aws.Bool(false),
			SpotInstanceRequestIds: requestIds,
		}

		err = svc.WaitUntilSpotInstanceRequestFulfilled(describeReq)

		if err != nil {
			logrus.Errorf("wait until request full filled %v", err)
		}

		spotRequests, err := svc.DescribeSpotInstanceRequests(describeReq)

		if err != nil {
			logrus.Errorf("describe spot instance requests %v", err)
		}

		logrus.Debugf("Tag spot instance requests and spot instances")
		for _, instance := range spotRequests.SpotInstanceRequests {

			ec2Tags := []*ec2.Tag{
				{
					Key:   aws.String("KubernetesCluster"),
					Value: aws.String(config.Kube.Name),
				},
				{
					Key:   aws.String(clouds.TagClusterID),
					Value: aws.String(config.Kube.ID),
				},
				{
					Key: aws.String("Name"),
					Value: aws.String(util.MakeNodeName(config.Kube.Name,
						uuid.New()[:4], config.IsMaster)),
				},
				{
					Key:   aws.String("Role"),
					Value: aws.String(util.MakeRole(config.IsMaster)),
				},
			}

			tagInput := &ec2.CreateTagsInput{
				Resources: []*string{},
				Tags:      ec2Tags,
			}

			logrus.Infof("Tag instance %s and request id %s",
				*instance.InstanceId, *instance.SpotInstanceRequestId)
			tagInput.Resources = append(tagInput.Resources, instance.InstanceId)
			tagInput.Resources = append(tagInput.Resources, instance.SpotInstanceRequestId)

			_, err = svc.CreateTags(tagInput)

			if err != nil {
				logrus.Errorf("tagging spot instances %v", err)
			}
		}
	}()

	return nil
}

func getAwsSpotPrices(machineType string, config *steps.Config) ([]string, error) {
	svc, err := amazon.GetEC2(config.AWSConfig)

	if err != nil {
		return nil, errors.Wrap(err, "get EC2 client")
	}

	spotPriceReq := &ec2.DescribeSpotPriceHistoryInput{
		AvailabilityZone: aws.String(config.AWSConfig.AvailabilityZone),
		EndTime:          aws.Time(time.Now()),
		StartTime:        aws.Time(time.Now().Add(time.Hour * -24 * 7)),
		InstanceTypes:    []*string{aws.String(machineType)},
	}

	prices, _ := svc.DescribeSpotPriceHistory(spotPriceReq)
	spotPrices := make([]string, 0)

	for _, spotPrice := range prices.SpotPriceHistory {
		if strings.EqualFold(*spotPrice.ProductDescription, "Linux/UNIX") {
			spotPrices = append(spotPrices, *spotPrice.SpotPrice)
		}
	}

	return spotPrices, nil
}

func findNextMinorVersion(current string, versions []string) string {
	if len(versions) == 0 {
		return ""
	}

	for i := 0; i < len(versions)-1; i++ {
		if (len(versions[i]) > 3 && len(current) > 3) && strings.EqualFold(versions[i][:4], current[:4]) {
			return versions[i+1]
		}
	}

	return ""
}

func discoverK8SVersion(kubeConfig *clientcmddapi.Config) (string, error) {
	restConf, err := clientcmd.NewNonInteractiveClientConfig(
		*kubeConfig,
		kubeConfig.CurrentContext,
		&clientcmd.ConfigOverrides{},
		nil,
	).ClientConfig()

	if err != nil {
		return "", errors.Wrapf(err, "create rest config")
	}

	restConf.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}
	if len(restConf.UserAgent) == 0 {
		restConf.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConf)

	if err != nil {
		return "", errors.Wrapf(err, "error create discovery client")
	}

	serverVersion, err := discoveryClient.ServerVersion()

	if err != nil {
		return "", errors.Wrapf(err, "error getting server version")
	}

	return strings.TrimPrefix(serverVersion.GitVersion, "v"), nil
}

func discoverHelmVersion(kubeConfig *clientcmddapi.Config) (string, error) {
	restConf, err := clientcmd.NewNonInteractiveClientConfig(
		*kubeConfig,
		kubeConfig.CurrentContext,
		&clientcmd.ConfigOverrides{},
		nil,
	).ClientConfig()

	if err != nil {
		return "", errors.Wrapf(err, "create rest config")
	}

	restConf.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}
	if len(restConf.UserAgent) == 0 {
		restConf.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	clientSet, err := kubernetes.NewForConfig(restConf)

	if err != nil {
		return "", errors.Wrapf(err, "get client set")
	}

	deploymentList, err := clientSet.AppsV1().Deployments("kube-system").List(v1.ListOptions{})

	if err != nil {
		return "", errors.Wrapf(err, "list deployments")
	}

	for _, deployment := range deploymentList.Items {
		if strings.Contains(deployment.Name, "tiller") {
			for _, container := range deployment.Spec.Template.Spec.Containers {
				slice := strings.Split(container.Image, ":")

				if len(slice) > 1 {
					return strings.Trim(slice[1], "v"), nil
				}
			}
		}
	}

	return "", nil
}
