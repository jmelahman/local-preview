data "aws_region" "current" {}

# Default VPC/subnet only when the caller doesn't pin one, so the example
# stands alone in a fresh account without pulling in a VPC module.
data "aws_vpc" "default" {
  count   = local.need_default_subnets ? 1 : 0
  default = true
}

data "aws_subnets" "default" {
  count = local.need_default_subnets ? 1 : 0

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
  # The load balancer needs subnets in two AZs, so TLS pulls in the default
  # VPC even when the caller pinned the instance's own subnet.
  need_default_subnets = var.subnet_id == null || (var.enable_tls && var.alb_subnet_ids == null)

  subnet_id      = var.subnet_id != null ? var.subnet_id : sort(data.aws_subnets.default[0].ids)[0]
  alb_subnet_ids = var.alb_subnet_ids != null ? var.alb_subnet_ids : data.aws_subnets.default[0].ids
  ami_id         = var.ami_id != null ? var.ami_id : data.aws_ami.al2023[0].id

  scheme = var.enable_tls ? "https" : "http"

  # $PREVIEW_CONFIG_DIR, bind-mounted read-only. Holds manifests/<repo>.toml.
  config_dir = "/etc/local-preview"

  tags = merge({
    Name      = var.name
    ManagedBy = "terraform"
  }, var.tags)

  # GitHub matches the OAuth App's callback URL exactly, so it has to be the
  # public origin — the load balancer's scheme and the preview domain, which
  # is what the module's certificate and records cover.
  sso_callback_url = var.sso == null ? null : coalesce(
    var.sso.callback_url,
    "${local.scheme}://${var.preview_domain}/api/auth/callback",
  )

  # Non-secret flags only. The client secret arrives through the env file
  # (PREVIEW_SSO_GITHUB_CLIENT_SECRET) so it stays out of `ps` and of state.
  sso_args = var.sso == null ? [] : concat(
    [
      "--sso-github-client-id", var.sso.github_client_id,
      "--sso-callback-url", local.sso_callback_url,
    ],
    var.sso.allowed_org != null ? ["--sso-allowed-org", var.sso.allowed_org] : [],
    var.sso.allowed_team != null ? ["--sso-allowed-team", var.sso.allowed_team] : [],
    length(var.sso.allowed_logins) > 0 ? ["--sso-allowed-logins", join(",", var.sso.allowed_logins)] : [],
    length(var.sso.allowed_emails) > 0 ? ["--sso-allowed-emails", join(",", var.sso.allowed_emails)] : [],
  )

  oidc_upload_args = var.github_oidc_audience == null ? [] : [
    "--github-oidc-audience", var.github_oidc_audience,
  ]

  # The server only ever sees plain HTTP, so left alone it hands out http://
  # preview URLs even when the load balancer serves them over TLS. That is
  # cosmetic until previews are gated — then the redirect handshake carries a
  # Secure cookie, which an http:// URL doesn't. A caller who sets the flag
  # in extra_server_args keeps their own value.
  base_url = var.enable_tls || var.http_port == 80 ? (
    "${local.scheme}://${var.preview_domain}"
  ) : "http://${var.preview_domain}:${var.http_port}"

  base_url_args = contains(var.extra_server_args, "--preview-base-url") ? [] : [
    "--preview-base-url", local.base_url,
  ]

  # With a worker tier, the server becomes the control node: builds stay here,
  # serving is routed to the workers. Endpoints are rendered from the literal
  # private_ips so user_data stays known at plan time.
  worker_endpoints = var.workers == null ? [] : [
    for ip in var.workers.private_ips : "http://${ip}:${var.workers.api_port}"
  ]
  worker_args = var.workers == null ? [] : [
    "--role", "control",
    "--worker-endpoints", join(",", local.worker_endpoints),
  ]

  server_args = concat(local.base_url_args, local.sso_args, local.oidc_upload_args, local.worker_args, var.extra_server_args)

  # All SecureString parameters the instances read at boot; none is a
  # Terraform value, so none reaches state.
  secret_ssm_parameters = distinct(concat(compact([
    var.github_webhook_secret_ssm_parameter,
    var.sso == null ? null : var.sso.client_secret_ssm_parameter,
    var.workers == null ? null : var.workers.secret_ssm_parameter,
  ]), values(var.secret_env_ssm_parameters)))

  # What the *worker* boot script reads: the shared secret plus the secret env
  # (a worker resolves manifest {secret:...} placeholders itself).
  worker_secret_ssm_parameters = var.workers == null ? [] : distinct(concat(
    [var.workers.secret_ssm_parameter],
    values(var.secret_env_ssm_parameters),
  ))
}

# The data volume is AZ-bound, so it has to land in the instance's AZ.
data "aws_subnet" "instance" {
  id = local.subnet_id
}

resource "aws_security_group" "server" {
  name        = var.name
  description = "Dashboard and preview ingress for ${var.name}"
  vpc_id      = data.aws_subnet.instance.vpc_id

  # Behind a load balancer the instance takes traffic from it and nothing
  # else; without one it is the front door itself.
  dynamic "ingress" {
    for_each = var.enable_tls ? [] : [1]

    content {
      description = "Dashboard and previews"
      from_port   = var.http_port
      to_port     = var.http_port
      protocol    = "tcp"
      cidr_blocks = var.allowed_ingress_cidrs
    }
  }

  dynamic "ingress" {
    for_each = var.enable_tls ? [1] : []

    content {
      description     = "Dashboard and previews, from the load balancer"
      from_port       = var.http_port
      to_port         = var.http_port
      protocol        = "tcp"
      security_groups = [aws_security_group.alb[0].id]
    }
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

# One read per configured secret parameter: the webhook secret, the SSO
# client secret, or both.
data "aws_iam_policy_document" "secrets" {
  count = length(local.secret_ssm_parameters) > 0 ? 1 : 0

  statement {
    actions = ["ssm:GetParameter"]
    resources = [
      for name in local.secret_ssm_parameters :
      "arn:aws:ssm:${data.aws_region.current.name}:*:parameter/${trimprefix(name, "/")}"
    ]
  }
}

resource "aws_iam_role_policy" "secrets" {
  count = length(local.secret_ssm_parameters) > 0 ? 1 : 0

  name   = "${var.name}-secrets"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.secrets[0].json
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
  private_ip                  = var.private_ip
  key_name                    = var.key_pair_name
  iam_instance_profile        = aws_iam_instance_profile.instance.name
  vpc_security_group_ids      = [aws_security_group.server.id]
  associate_public_ip_address = true

  # Gzipped, which cloud-init unpacks itself: EC2 caps user data at 16 KiB and
  # local_manifests/compose_stacks are embedded in it, so plain text runs out
  # of room after a couple of repos.
  user_data_base64 = base64gzip(templatefile("${path.module}/user-data.sh.tftpl", {
    aws_region      = data.aws_region.current.name
    compose_stacks  = var.compose_stacks
    compose_version = var.docker_compose_version
    config_dir      = local.config_dir
    data_dir        = var.data_dir
    data_volume_id  = aws_ebs_volume.data.id
    http_port       = var.http_port
    image           = var.image
    local_manifests = var.local_manifests
    preview_domain  = var.preview_domain
    server_args     = join(" ", local.server_args)
    webhook_ssm     = var.github_webhook_secret_ssm_parameter == null ? "" : var.github_webhook_secret_ssm_parameter
    sso_secret_ssm  = var.sso == null ? "" : var.sso.client_secret_ssm_parameter
    worker_ssm      = var.workers == null ? "" : var.workers.secret_ssm_parameter
    secret_env      = var.secret_env_ssm_parameters
  }))

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

# ---------------------------------------------------------------------------
# TLS. The server speaks only HTTP, so the certificate lives on a load
# balancer in front of it. Preview hosts are one label deep, so a single
# wildcard certificate covers every repo.
# ---------------------------------------------------------------------------

resource "aws_security_group" "alb" {
  count = var.enable_tls ? 1 : 0

  name        = "${var.name}-alb"
  description = "Public ingress for ${var.name}"
  vpc_id      = data.aws_subnet.instance.vpc_id

  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = var.allowed_ingress_cidrs
  }

  ingress {
    description = "HTTP, redirected to HTTPS"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = var.allowed_ingress_cidrs
  }

  egress {
    description = "To the server, and to the IdP when OIDC is on"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = local.tags
}

resource "aws_acm_certificate" "previews" {
  count = var.enable_tls ? 1 : 0

  domain_name               = var.preview_domain
  subject_alternative_names = ["*.${var.preview_domain}"]
  validation_method         = "DNS"
  tags                      = local.tags

  lifecycle {
    create_before_destroy = true

    precondition {
      condition     = var.route53_zone_id != null
      error_message = "enable_tls needs route53_zone_id: the certificate is DNS-validated. Set enable_tls = false to serve plain HTTP instead."
    }
  }
}

# Keyed by domain name, the one part of domain_validation_options known at
# plan time. The apex and the wildcard validate to the same CNAME, so the two
# instances write the same record — hence allow_overwrite.
resource "aws_route53_record" "cert_validation" {
  for_each = var.enable_tls ? {
    for dvo in aws_acm_certificate.previews[0].domain_validation_options :
    dvo.domain_name => dvo
  } : {}

  zone_id         = var.route53_zone_id
  name            = each.value.resource_record_name
  type            = each.value.resource_record_type
  records         = [each.value.resource_record_value]
  ttl             = 60
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "previews" {
  count = var.enable_tls ? 1 : 0

  certificate_arn         = aws_acm_certificate.previews[0].arn
  validation_record_fqdns = [for r in aws_route53_record.cert_validation : r.fqdn]
}

resource "aws_lb" "server" {
  count = var.enable_tls ? 1 : 0

  name               = var.name
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb[0].id]
  subnets            = local.alb_subnet_ids

  # Build logs stream, so don't cut idle connections at the 60s default.
  idle_timeout = 300

  drop_invalid_header_fields = true

  # Only the ALB sees requests it turned away, so this is the sole record of
  # traffic that never reached the server.
  dynamic "access_logs" {
    for_each = var.access_logs_bucket == null ? [] : [var.access_logs_bucket]
    content {
      bucket  = access_logs.value
      prefix  = var.access_logs_prefix
      enabled = true
    }
  }

  tags = local.tags
}

resource "aws_lb_target_group" "server" {
  count = var.enable_tls ? 1 : 0

  name        = var.name
  port        = var.http_port
  protocol    = "HTTP"
  target_type = "instance"
  vpc_id      = data.aws_subnet.instance.vpc_id

  health_check {
    path    = "/api/health"
    matcher = "200"
  }

  tags = local.tags
}

resource "aws_lb_target_group_attachment" "server" {
  count = var.enable_tls ? 1 : 0

  target_group_arn = aws_lb_target_group.server[0].arn
  target_id        = aws_instance.server.id
  port             = var.http_port
}

resource "aws_lb_listener" "https" {
  count = var.enable_tls ? 1 : 0

  load_balancer_arn = aws_lb.server[0].arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate_validation.previews[0].certificate_arn

  # OIDC first when configured: without it the server is unauthenticated to
  # anyone the security group lets through.
  dynamic "default_action" {
    for_each = var.oidc != null ? [var.oidc] : []

    content {
      type  = "authenticate-oidc"
      order = 1

      authenticate_oidc {
        issuer                 = default_action.value.issuer
        authorization_endpoint = default_action.value.authorization_endpoint
        token_endpoint         = default_action.value.token_endpoint
        user_info_endpoint     = default_action.value.user_info_endpoint
        client_id              = default_action.value.client_id
        client_secret          = default_action.value.client_secret
        scope                  = default_action.value.scope
        session_timeout        = default_action.value.session_timeout
      }
    }
  }

  default_action {
    type             = "forward"
    order            = var.oidc != null ? 2 : null
    target_group_arn = aws_lb_target_group.server[0].arn
  }
}

# The CLI and webhook senders can't follow a login redirect, so they reach
# the server directly from known ranges instead.
resource "aws_lb_listener_rule" "oidc_bypass" {
  count = var.enable_tls && var.oidc != null && length(var.oidc_bypass_cidrs) > 0 ? 1 : 0

  listener_arn = aws_lb_listener.https[0].arn
  priority     = 1

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.server[0].arn
  }

  condition {
    source_ip {
      values = var.oidc_bypass_cidrs
    }
  }
}

resource "aws_lb_listener" "http" {
  count = var.enable_tls ? 1 : 0

  load_balancer_arn = aws_lb.server[0].arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"

    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}

# A static address, so the DNS records survive an instance replacement.
resource "aws_eip" "server" {
  instance = aws_instance.server.id
  domain   = "vpc"
  tags     = local.tags
}

# Two records regardless of how many repos are registered: the dashboard
# answers anything whose Host isn't a preview, and preview hosts are
# <sha>-<repo>.<preview_domain> — a single label, so one wildcard covers
# every repo and registering one needs no DNS change.
#
# They point at the load balancer when it terminates TLS, and straight at the
# instance otherwise.
locals {
  dns_names = var.route53_zone_id == null ? [] : [var.preview_domain, "*.${var.preview_domain}"]
}

resource "aws_route53_record" "server" {
  for_each = toset(local.dns_names)

  zone_id = var.route53_zone_id
  name    = each.key
  type    = "A"

  ttl     = var.enable_tls ? null : 300
  records = var.enable_tls ? null : [aws_eip.server.public_ip]

  dynamic "alias" {
    for_each = var.enable_tls ? [1] : []

    content {
      name                   = aws_lb.server[0].dns_name
      zone_id                = aws_lb.server[0].zone_id
      evaluate_target_health = false
    }
  }
}

# ---------------------------------------------------------------------------
# Worker tier. Same image, --role=worker: the control node above routes
# preview serving here over the internal worker API. Workers are disposable —
# no EBS data volume, no builds, no manifests; artifact files hydrate from
# the S3 tier and run specs arrive on the wire. The worker API is remote code
# execution by design: it listens on the worker's private IP and admits only
# the server's security group. Never attach it to a load balancer.
# ---------------------------------------------------------------------------

resource "aws_security_group" "worker" {
  count = var.workers == null ? 0 : 1

  name        = "${var.name}-worker"
  description = "Worker tier for ${var.name}: control-node-only ingress"
  vpc_id      = data.aws_subnet.instance.vpc_id

  ingress {
    description     = "Worker API, from the control node only (RCE surface)"
    from_port       = var.workers.api_port
    to_port         = var.workers.api_port
    protocol        = "tcp"
    security_groups = [aws_security_group.server.id]
  }

  ingress {
    description     = "Proxied preview processes (OS-assigned ports), from the control node only"
    from_port       = 1024
    to_port         = 65535
    protocol        = "tcp"
    security_groups = [aws_security_group.server.id]
  }

  egress {
    description = "All egress: image pulls and artifact hydration"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.tags, { Name = "${var.name}-worker" })
}

# How workers reach the shared dependency stack: the stack publishes its
# ports on the server's (static) private IP, and these rules are the only
# ingress to them. Separate rule resources so the base SG needn't change.
resource "aws_vpc_security_group_ingress_rule" "stack_from_workers" {
  for_each = var.workers == null ? toset([]) : toset([for p in var.workers.stack_ingress_ports : tostring(p)])

  security_group_id            = aws_security_group.server.id
  description                  = "Shared dependency stack port, from the worker tier"
  from_port                    = tonumber(each.value)
  to_port                      = tonumber(each.value)
  ip_protocol                  = "tcp"
  referenced_security_group_id = aws_security_group.worker[0].id

  tags = local.tags
}

resource "aws_iam_role" "worker" {
  count = var.workers == null ? 0 : 1

  name               = "${var.name}-worker"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "worker_ssm_core" {
  count = var.workers == null ? 0 : 1

  role       = aws_iam_role.worker[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "worker_secrets" {
  count = var.workers == null ? 0 : 1

  statement {
    actions = ["ssm:GetParameter"]
    resources = [
      for name in local.worker_secret_ssm_parameters :
      "arn:aws:ssm:${data.aws_region.current.name}:*:parameter/${trimprefix(name, "/")}"
    ]
  }
}

resource "aws_iam_role_policy" "worker_secrets" {
  count = var.workers == null ? 0 : 1

  name   = "${var.name}-worker-secrets"
  role   = aws_iam_role.worker[0].id
  policy = data.aws_iam_policy_document.worker_secrets[0].json
}

resource "aws_iam_instance_profile" "worker" {
  count = var.workers == null ? 0 : 1

  name = "${var.name}-worker"
  role = aws_iam_role.worker[0].name
  tags = local.tags
}

resource "aws_instance" "worker" {
  count = var.workers == null ? 0 : length(var.workers.private_ips)

  ami                    = local.ami_id
  instance_type          = var.workers.instance_type
  subnet_id              = local.subnet_id
  private_ip             = var.workers.private_ips[count.index]
  iam_instance_profile   = aws_iam_instance_profile.worker[0].name
  vpc_security_group_ids = [aws_security_group.worker[0].id]
  # Public IP for egress only (image pulls, S3 hydration): the default VPC has
  # no NAT, and the security group admits nothing inbound but the control node.
  associate_public_ip_address = true

  user_data_base64 = base64gzip(templatefile("${path.module}/worker-data.sh.tftpl", {
    aws_region  = data.aws_region.current.name
    image       = var.image
    data_dir    = var.data_dir
    private_ip  = var.workers.private_ips[count.index]
    api_port    = var.workers.api_port
    max_warm    = var.workers.max_warm
    server_args = join(" ", var.workers.extra_server_args)
    worker_ssm  = var.workers.secret_ssm_parameter
    secret_env  = var.secret_env_ssm_parameters
  }))
  user_data_replace_on_change = true

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  root_block_device {
    volume_size           = var.workers.root_volume_size_gb
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }

  tags = merge(local.tags, { Name = "${var.name}-worker-${count.index}" })
}
