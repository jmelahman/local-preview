data "aws_region" "current" {}

# Default VPC/subnet only when the caller doesn't pin one, so the example
# stands alone in a fresh account without pulling in a VPC module.
data "aws_vpc" "default" {
  count   = var.subnet_id == null ? 1 : 0
  default = true
}

data "aws_subnets" "default" {
  count = var.subnet_id == null ? 1 : 0

  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default[0].id]
  }
}

data "aws_ami" "al2023" {
  count       = var.ami_id == null ? 1 : 0
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-x86_64"]
  }
}

locals {
  subnet_id = var.subnet_id != null ? var.subnet_id : sort(data.aws_subnets.default[0].ids)[0]
  ami_id    = var.ami_id != null ? var.ami_id : data.aws_ami.al2023[0].id

  tags = merge({
    Name      = var.name
    ManagedBy = "terraform"
  }, var.tags)
}

# The data volume is AZ-bound, so it has to land in the instance's AZ.
data "aws_subnet" "instance" {
  id = local.subnet_id
}

resource "aws_security_group" "server" {
  name        = var.name
  description = "Dashboard and preview ingress for ${var.name}"
  vpc_id      = data.aws_subnet.instance.vpc_id

  ingress {
    description = "Dashboard and previews"
    from_port   = var.http_port
    to_port     = var.http_port
    protocol    = "tcp"
    cidr_blocks = var.allowed_ingress_cidrs
  }

  dynamic "ingress" {
    for_each = var.allow_ssh ? [1] : []

    content {
      description = "SSH"
      from_port   = 22
      to_port     = 22
      protocol    = "tcp"
      cidr_blocks = var.allowed_ingress_cidrs
    }
  }

  egress {
    description = "All egress: target builds fetch their own dependencies"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = local.tags
}

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "instance" {
  name               = var.name
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
  tags               = local.tags
}

# Session Manager, so the module needn't open port 22 to get a shell.
resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "webhook_secret" {
  count = var.github_webhook_secret_ssm_parameter != null ? 1 : 0

  statement {
    actions   = ["ssm:GetParameter"]
    resources = ["arn:aws:ssm:${data.aws_region.current.name}:*:parameter/${trimprefix(var.github_webhook_secret_ssm_parameter, "/")}"]
  }
}

resource "aws_iam_role_policy" "webhook_secret" {
  count = var.github_webhook_secret_ssm_parameter != null ? 1 : 0

  name   = "${var.name}-webhook-secret"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.webhook_secret[0].json
}

resource "aws_iam_instance_profile" "instance" {
  name = var.name
  role = aws_iam_role.instance.name
  tags = local.tags
}

resource "aws_ebs_volume" "data" {
  availability_zone = data.aws_subnet.instance.availability_zone
  size              = var.data_volume_size_gb
  type              = "gp3"
  encrypted         = true

  tags = merge(local.tags, { Name = "${var.name}-data" })

  # Artifacts, mirror clones, and per-artifact state live here; losing it
  # means every registered repo has to be re-registered and rebuilt.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_instance" "server" {
  ami                         = local.ami_id
  instance_type               = var.instance_type
  subnet_id                   = local.subnet_id
  key_name                    = var.key_pair_name
  iam_instance_profile        = aws_iam_instance_profile.instance.name
  vpc_security_group_ids      = [aws_security_group.server.id]
  associate_public_ip_address = true

  user_data = templatefile("${path.module}/user-data.sh.tftpl", {
    aws_region     = data.aws_region.current.name
    data_dir       = var.data_dir
    data_volume_id = aws_ebs_volume.data.id
    http_port      = var.http_port
    image          = var.image
    preview_domain = var.preview_domain
    server_args    = join(" ", var.extra_server_args)
    webhook_ssm    = var.github_webhook_secret_ssm_parameter == null ? "" : var.github_webhook_secret_ssm_parameter
  })

  # The image is baked into the unit file that user_data writes, so an image
  # bump only takes effect if the instance is rebuilt. The data volume is a
  # separate resource and survives.
  user_data_replace_on_change = true

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  root_block_device {
    volume_size           = var.root_volume_size_gb
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }

  tags = local.tags
}

resource "aws_volume_attachment" "data" {
  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.data.id
  instance_id = aws_instance.server.id
}

# A static address, so the DNS records survive an instance replacement.
resource "aws_eip" "server" {
  instance = aws_instance.server.id
  domain   = "vpc"
  tags     = local.tags
}

# The dashboard: anything whose Host isn't *.<preview_domain> gets it.
resource "aws_route53_record" "dashboard" {
  count = var.route53_zone_id != null ? 1 : 0

  zone_id = var.route53_zone_id
  name    = var.preview_domain
  type    = "A"
  ttl     = 300
  records = [aws_eip.server.public_ip]
}

# One wildcard for every repo: preview hosts are <sha>-<repo>.<preview_domain>,
# a single label, so this record answers for all of them. Registering a repo
# needs no DNS change.
resource "aws_route53_record" "previews" {
  count = var.route53_zone_id != null ? 1 : 0

  zone_id = var.route53_zone_id
  name    = "*.${var.preview_domain}"
  type    = "A"
  ttl     = 300
  records = [aws_eip.server.public_ip]
}
