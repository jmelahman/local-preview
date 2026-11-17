locals {
  ingestion_asg_name = "${var.name}-ingestion"
  ingestion_args = var.ingestion_workers == null ? [] : [
    "--ingestion-control-listen", "${var.private_ip}:9102",
    "--ingestion-asg-name", local.ingestion_asg_name,
    "--aws-region", data.aws_region.current.name,
    "--metrics-namespace", local.metrics_namespace,
  ]
  ingestion_secret_env = var.ingestion_workers == null ? {} : {
    PREVIEW_INGESTION_SECRET = var.ingestion_workers.secret_ssm_parameter
  }
  ingestion_instance_types = var.ingestion_workers == null ? [] : (
    length(var.ingestion_workers.instance_types) > 0 ? var.ingestion_workers.instance_types : [var.ingestion_workers.instance_type]
  )
}

resource "aws_security_group" "ingestion" {
  count = var.ingestion_workers == null ? 0 : 1

  name        = "${var.name}-ingestion"
  description = "Outbound-only ingestion workers"
  ingress     = []
  vpc_id      = data.aws_subnet.instance.vpc_id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = local.tags
}

resource "aws_security_group" "ingestion_control" {
  count = var.ingestion_workers == null ? 0 : 1

  name        = "${var.name}-ingestion-control"
  description = "Private ingestion control API, from ingestion workers only"
  vpc_id      = data.aws_subnet.instance.vpc_id

  ingress {
    from_port       = 9102
    to_port         = 9102
    protocol        = "tcp"
    security_groups = [aws_security_group.ingestion[0].id]
  }

  tags = local.tags
}

resource "aws_iam_role" "ingestion" {
  count = var.ingestion_workers == null ? 0 : 1

  name               = "${var.name}-ingestion"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "ingestion_ssm_core" {
  count = var.ingestion_workers == null ? 0 : 1

  role       = aws_iam_role.ingestion[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "ingestion" {
  count = var.ingestion_workers == null ? 0 : 1

  statement {
    actions = ["ssm:GetParameter"]
    resources = [
      "arn:aws:ssm:${data.aws_region.current.name}:*:parameter/${trimprefix(var.ingestion_workers.secret_ssm_parameter, "/")}",
    ]
  }

  statement {
    actions = ["autoscaling:SetInstanceProtection", "autoscaling:TerminateInstanceInAutoScalingGroup"]
    resources = [
      "arn:aws:autoscaling:${data.aws_region.current.name}:*:autoScalingGroup:*:autoScalingGroupName/${local.ingestion_asg_name}",
    ]
  }

  statement {
    actions   = ["autoscaling:DescribeAutoScalingGroups", "autoscaling:DescribeAutoScalingInstances"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "ingestion" {
  count = var.ingestion_workers == null ? 0 : 1

  name   = "${var.name}-ingestion"
  role   = aws_iam_role.ingestion[0].id
  policy = data.aws_iam_policy_document.ingestion[0].json
}

resource "aws_iam_instance_profile" "ingestion" {
  count = var.ingestion_workers == null ? 0 : 1

  name = "${var.name}-ingestion"
  role = aws_iam_role.ingestion[0].name
  tags = local.tags
}

resource "aws_launch_template" "ingestion" {
  count = var.ingestion_workers == null ? 0 : 1

  name_prefix   = "${var.name}-ingestion-"
  image_id      = local.ami_id
  instance_type = var.ingestion_workers.instance_type

  iam_instance_profile {
    name = aws_iam_instance_profile.ingestion[0].name
  }

  network_interfaces {
    associate_public_ip_address = true
    security_groups             = [aws_security_group.ingestion[0].id]
    delete_on_termination       = true
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  block_device_mappings {
    device_name = "/dev/xvda"

    ebs {
      volume_size           = var.ingestion_workers.root_volume_size_gb
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = true
    }
  }

  user_data = base64gzip(templatefile("${path.module}/ingestion-data.sh.tftpl", {
    aws_region       = data.aws_region.current.name
    image            = var.image
    control_endpoint = "http://${var.private_ip}:9102"
    secret_ssm       = var.ingestion_workers.secret_ssm_parameter
    memory_limit     = var.ingestion_workers.memory_limit
    cpus             = var.ingestion_workers.cpus
    asg_name         = local.ingestion_asg_name
    server_args      = join(" ", [for a in var.ingestion_workers.extra_server_args : "\"${a}\""])
  }))

  update_default_version = true

  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { Name = local.ingestion_asg_name })
  }

  lifecycle {
    precondition {
      condition     = var.private_ip != null
      error_message = "ingestion_workers requires a fixed private_ip for its private control endpoint."
    }
  }

  tags = local.tags
}

resource "aws_autoscaling_group" "ingestion" {
  count = var.ingestion_workers == null ? 0 : 1

  name                      = local.ingestion_asg_name
  min_size                  = var.ingestion_workers.min_size
  max_size                  = var.ingestion_workers.max_size
  desired_capacity          = var.ingestion_workers.desired_capacity
  vpc_zone_identifier       = [local.subnet_id]
  health_check_type         = "EC2"
  wait_for_capacity_timeout = "0"

  mixed_instances_policy {
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.ingestion[0].id
        version            = "$Latest"
      }

      dynamic "override" {
        for_each = local.ingestion_instance_types
        content {
          instance_type = override.value
        }
      }
    }

    instances_distribution {
      on_demand_percentage_above_base_capacity = 100
    }
  }

  lifecycle {
    ignore_changes = [desired_capacity]
  }

  tag {
    key                 = "Name"
    value               = local.ingestion_asg_name
    propagate_at_launch = true
  }

  dynamic "tag" {
    for_each = { for k, v in local.tags : k => v if k != "Name" }
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }
}

resource "aws_autoscaling_policy" "ingestion_demand" {
  count = var.ingestion_workers == null ? 0 : 1

  name                      = "${var.name}-ingestion-scale-out"
  autoscaling_group_name    = aws_autoscaling_group.ingestion[0].name
  policy_type               = "StepScaling"
  adjustment_type           = "ChangeInCapacity"
  metric_aggregation_type   = "Maximum"
  estimated_instance_warmup = 300

  dynamic "step_adjustment" {
    for_each = toset([1, 2, 3, 4])
    content {
      metric_interval_lower_bound = step_adjustment.value - 1
      metric_interval_upper_bound = step_adjustment.value == 4 ? null : step_adjustment.value
      scaling_adjustment          = step_adjustment.value
    }
  }
}

resource "aws_cloudwatch_metric_alarm" "ingestion_queued" {
  count = var.ingestion_workers == null ? 0 : 1

  alarm_name          = "${var.name}-ingestion-queued"
  alarm_description   = "Queued ingestion jobs need an isolated worker, including from zero capacity."
  namespace           = local.metrics_namespace
  metric_name         = "IngestionQueued"
  dimensions          = { Role = "control", IngestionGroup = local.ingestion_asg_name }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = 1
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_autoscaling_policy.ingestion_demand[0].arn]
  tags                = local.tags
}

data "aws_iam_policy_document" "ingestion_metrics" {
  count = var.ingestion_workers == null ? 0 : 1

  statement {
    actions   = ["cloudwatch:PutMetricData"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "cloudwatch:namespace"
      values   = [local.metrics_namespace]
    }
  }
}

resource "aws_iam_role_policy" "ingestion_metrics" {
  count = var.ingestion_workers == null ? 0 : 1

  name   = "${var.name}-ingestion-metrics"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.ingestion_metrics[0].json
}
