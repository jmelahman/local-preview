mock_provider "aws" {
  mock_data "aws_region" {
    defaults = { name = "us-west-2" }
  }

  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }

  mock_data "aws_subnet" {
    defaults = { vpc_id = "vpc-12345678", availability_zone = "us-west-2a" }
  }
}

variables {
  name                  = "test-preview"
  preview_domain        = "preview.example.test"
  allowed_ingress_cidrs = ["192.0.2.0/24"]
  subnet_id             = "subnet-12345678"
  private_ip            = "10.0.0.10"
  ami_id                = "ami-1234567890abcdef0"
  alb_subnet_ids        = ["subnet-12345678", "subnet-23456789"]
  enable_tls            = false
}

run "disabled_by_default" {
  command = plan

  assert {
    condition     = length(aws_autoscaling_group.ingestion) == 0 && length(local.ingestion_args) == 0
    error_message = "The ingestion pool must be absent unless configured."
  }
}

run "isolated_ingestion_pool" {
  command = plan

  variables {
    ingestion_workers = {
      secret_ssm_parameter = "/local-preview/ingestion-secret"
      extra_server_args    = ["--s3-endpoint", "localhost:9000", "--s3-bucket", "artifacts"]
    }
  }

  assert {
    condition     = aws_autoscaling_group.ingestion[0].min_size == 0 && aws_autoscaling_group.ingestion[0].max_size == 4
    error_message = "Ingestion capacity must scale independently from zero with an explicit maximum."
  }

  assert {
    condition     = length(aws_security_group.ingestion[0].ingress) == 0
    error_message = "Ingestion workers must have no inbound listener."
  }

  assert {
    condition     = alltrue([for rule in aws_security_group.ingestion_control[0].ingress : rule.from_port == 9102 && rule.to_port == 9102 && try(length(rule.cidr_blocks), 0) == 0 && length(rule.security_groups) == 1])
    error_message = "The private ingestion control API must not admit public CIDRs."
  }

  assert {
    condition     = aws_launch_template.ingestion[0].metadata_options[0].http_tokens == "required" && aws_launch_template.ingestion[0].metadata_options[0].http_put_response_hop_limit == 1
    error_message = "Build containers must not inherit the ingestion instance role through IMDS."
  }

  assert {
    condition     = aws_launch_template.ingestion[0].block_device_mappings[0].ebs[0].volume_size == 80 && aws_launch_template.ingestion[0].block_device_mappings[0].ebs[0].encrypted
    error_message = "Ingestion scratch storage must have a fixed encrypted size."
  }

  assert {
    condition     = aws_cloudwatch_metric_alarm.ingestion_queued[0].dimensions["Role"] == "control" && aws_cloudwatch_metric_alarm.ingestion_queued[0].dimensions["IngestionGroup"] == "test-preview-ingestion"
    error_message = "Ingestion demand must be scoped to this worker pool."
  }

  assert {
    condition = toset([for step in aws_autoscaling_policy.ingestion_demand[0].step_adjustment : step.scaling_adjustment]) == toset([1, 2, 3, 4]) && alltrue([
      for step in aws_autoscaling_policy.ingestion_demand[0].step_adjustment :
      tonumber(step.metric_interval_lower_bound) == step.scaling_adjustment - 1 && (
        step.scaling_adjustment == 4 ? step.metric_interval_upper_bound == null : tonumber(step.metric_interval_upper_bound) == step.scaling_adjustment
      )
    ]) && aws_cloudwatch_metric_alarm.ingestion_queued[0].threshold == 1 && aws_cloudwatch_metric_alarm.ingestion_queued[0].comparison_operator == "GreaterThanOrEqualToThreshold"
    error_message = "Queue counts must map to +1, +2, +3, or +4 workers, bounded by ASG capacity."
  }

  assert {
    condition     = local.ingestion_secret_env["PREVIEW_INGESTION_SECRET"] == "/local-preview/ingestion-secret" && length(local.worker_secret_ssm_parameters) == 0 && contains(local.secret_ssm_parameters, "/local-preview/ingestion-secret")
    error_message = "The ingestion secret must not become a serving-worker credential."
  }

  assert {
    condition = length(regexall("PREVIEW_INGESTION_SECRET", templatefile("${path.module}/user-data.sh.tftpl", {
      aws_region    = "us-west-2", compose_stacks = { dependencies = "services: {}" }, compose_version = "v2.36.0",
      config_dir    = "/etc/local-preview", data_dir = "/var/lib/local-preview", data_volume_id = "vol-test",
      http_port     = 80, image = "test:fixture", local_manifests = {}, preview_domain = "preview.test",
      server_args   = "", webhook_ssm = "", sso_secret_ssm = "", worker_ssm = "", secret_env = {},
      ingestion_ssm = "/local-preview/ingestion-secret"
    }))) == 1
    error_message = "The ingestion secret must be written only to the service env file, never Compose env files."
  }

  assert {
    condition = alltrue([for expected in ["Restart=on-failure", "--min-free-bytes 10737418240", "--memory 8g --memory-swap 8g --cpus 2", "--build-memory-bytes 2147483648 --build-nano-cpus 2000000000"] :
      strcontains(templatefile("${path.module}/ingestion-data.sh.tftpl", {
        aws_region = "us-west-2", image = "test-image", control_endpoint = "http://10.0.0.10:9102",
        secret_ssm = "/local-preview/ingestion-secret", memory_limit = "8g", cpus = 2,
        asg_name   = "test-preview-ingestion", server_args = ""
      }), expected)
    ])
    error_message = "Ingestion bootstrap must enforce budgets and preserve successful idle shutdown."
  }

}

run "invalid_capacity" {
  command = plan

  variables {
    ingestion_workers = {
      secret_ssm_parameter = "/local-preview/ingestion-secret"
      extra_server_args    = ["--s3-endpoint", "localhost:9000", "--s3-bucket", "artifacts"]
      min_size             = 2
      max_size             = 1
    }
  }

  expect_failures = [var.ingestion_workers]
}

run "unsafe_arguments" {
  command = plan

  variables {
    ingestion_workers = {
      secret_ssm_parameter = "/local-preview/ingestion-secret"
      extra_server_args    = ["--s3-endpoint", "localhost:9000", "--s3-bucket", "artifacts", "--value", "$SHELL"]
    }
  }

  expect_failures = [var.ingestion_workers]
}

run "missing_s3_configuration" {
  command = plan

  variables {
    ingestion_workers = {
      secret_ssm_parameter = "/local-preview/ingestion-secret"
    }
  }

  expect_failures = [var.ingestion_workers]
}
