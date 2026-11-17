package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/spf13/cobra"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/ingest"
	"github.com/jmelahman/local-preview/internal/s3store"
)

func ingestionWorkerCmd() *cobra.Command {
	var server, secret, dataDir, instanceID, asg, region string
	var endpoint, bucket, prefix, s3Region, accessKey, secretKey string
	var useTLS bool
	var buildMemory, buildCPU, minFree int64
	var idleTimeout time.Duration
	cmd := &cobra.Command{Use: "ingest-worker", Short: "Run one isolated ingestion job at a time"}
	cmd.Flags().StringVar(&server, "server", "", "Private ingestion coordinator URL")
	cmd.Flags().StringVar(&secret, "secret", "", "Ingestion secret (default: $PREVIEW_INGESTION_SECRET)")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "Dedicated ingestion scratch and mirror directory")
	cmd.Flags().StringVar(&instanceID, "instance-id", "", "EC2 instance ID for lease protection")
	cmd.Flags().StringVar(&asg, "asg-name", "", "Ingestion Auto Scaling group")
	cmd.Flags().StringVar(&region, "aws-region", "", "Region for ingestion instance protection")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 5*time.Minute, "Idle time before an autoscaled worker terminates")
	cmd.Flags().Int64Var(&buildMemory, "build-memory-bytes", 2<<30, "Hard memory limit for each build container")
	cmd.Flags().Int64Var(&buildCPU, "build-nano-cpus", 2_000_000_000, "CPU limit in nanocpus for each build container")
	cmd.Flags().Int64Var(&minFree, "min-free-bytes", 10<<30, "Required free scratch bytes before claiming a job")
	cmd.Flags().StringVar(&endpoint, "s3-endpoint", "", "Artifact object-store endpoint")
	cmd.Flags().StringVar(&bucket, "s3-bucket", "", "Artifact object-store bucket")
	cmd.Flags().StringVar(&prefix, "s3-prefix", "", "Canonical artifact prefix")
	cmd.Flags().StringVar(&s3Region, "s3-region", "", "Artifact object-store region")
	cmd.Flags().StringVar(&accessKey, "s3-access-key", "", "Object-store access key (default: ambient credentials)")
	cmd.Flags().StringVar(&secretKey, "s3-secret-key", "", "Object-store secret key")
	cmd.Flags().BoolVar(&useTLS, "s3-use-ssl", true, "Use TLS for object storage")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		envDefault(&secret, "PREVIEW_INGESTION_SECRET")
		if server == "" || secret == "" || dataDir == "" || buildMemory <= 0 || buildCPU <= 0 || minFree < 0 {
			return errors.New("ingest-worker requires a server, secret, data directory, and positive build limits")
		}
		if (asg == "") != (instanceID == "") {
			return errors.New("ingestion ASG and instance ID must be set together")
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return err
		}
		tier, err := s3store.New(s3store.Config{Endpoint: endpoint, Bucket: bucket, Prefix: prefix, Region: s3Region, AccessKey: accessKey, SecretKey: secretKey, UseSSL: useTLS, TmpDir: dataDir})
		if err != nil {
			return err
		}
		id := instanceID
		if id == "" {
			id, err = os.Hostname()
			if err != nil {
				return err
			}
		}
		worker := &ingest.Worker{ServerURL: server, Secret: secret, ID: id, DataDir: dataDir, Tier: tier, Executor: build.Executor{DataDir: dataDir, Runner: build.NewLimitedRunner(buildMemory, buildCPU), MaxExtractBytes: 2 << 30}, IdleTimeout: idleTimeout}
		worker.CheckCapacity = func(context.Context) error {
			free, err := filesystemFreeBytes(dataDir)
			if err != nil {
				return err
			}
			if free < minFree {
				return fmt.Errorf("ingestion scratch requires %d free bytes; %d are available", minFree, free)
			}
			return nil
		}
		if asg != "" {
			cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
			if err != nil {
				return err
			}
			client := autoscaling.NewFromConfig(cfg)
			protect := func(ctx context.Context, value bool) error {
				_, err := client.SetInstanceProtection(ctx, &autoscaling.SetInstanceProtectionInput{AutoScalingGroupName: aws.String(asg), InstanceIds: []string{instanceID}, ProtectedFromScaleIn: aws.Bool(value)})
				return err
			}
			worker.BeforeJob = func(ctx context.Context) error { return protect(ctx, true) }
			worker.AfterJob = func(ctx context.Context) error { return protect(ctx, false) }
			worker.OnIdle = func(ctx context.Context) error {
				groups, err := client.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{asg}})
				if err != nil {
					return err
				}
				if len(groups.AutoScalingGroups) != 1 {
					return errors.New("ingestion Auto Scaling group was not found")
				}
				group := groups.AutoScalingGroups[0]
				if aws.ToInt32(group.DesiredCapacity) <= aws.ToInt32(group.MinSize) {
					return errors.New("ingestion group is at its minimum capacity")
				}
				if err := protect(ctx, false); err != nil {
					return err
				}
				_, err = client.TerminateInstanceInAutoScalingGroup(ctx, &autoscaling.TerminateInstanceInAutoScalingGroupInput{InstanceId: aws.String(instanceID), ShouldDecrementDesiredCapacity: aws.Bool(true)})
				return err
			}
		}
		err = worker.Run(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return cmd
}

func ingestionMetrics(ctx context.Context, region, namespace, group string) (func(context.Context, db.JobStats) error, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		namespace = "LocalPreview"
	}
	client := cloudwatch.NewFromConfig(cfg)
	return func(ctx context.Context, stats db.JobStats) error {
		metrics := make([]cwtypes.MetricDatum, 0, 3)
		for name, value := range map[string]float64{"IngestionQueued": float64(stats.Queued), "IngestionActive": float64(stats.Active), "IngestionOldestQueuedAge": float64(stats.OldestQueuedAgeSeconds)} {
			metrics = append(metrics, cwtypes.MetricDatum{MetricName: aws.String(name), Value: aws.Float64(value), Dimensions: []cwtypes.Dimension{{Name: aws.String("Role"), Value: aws.String("control")}, {Name: aws.String("IngestionGroup"), Value: aws.String(group)}}})
		}
		_, err := client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{Namespace: aws.String(namespace), MetricData: metrics})
		return err
	}, nil
}
