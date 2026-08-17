variable "name" {
  description = "Name prefix for every resource this module creates."
  type        = string
  default     = "local-preview"
}

variable "allowed_ingress_cidrs" {
  description = <<-EOT
    CIDRs allowed to reach the dashboard and previews. Required, with no
    default, on purpose: the server has no authentication, and anyone who
    can reach the dashboard can register a repo and make the host run that
    repo's build commands. Keep this to a VPN/office range.
  EOT
  type        = list(string)

  validation {
    condition     = length(var.allowed_ingress_cidrs) > 0
    error_message = "allowed_ingress_cidrs must not be empty — the server has no auth of its own."
  }

  validation {
    condition     = !contains(var.allowed_ingress_cidrs, "0.0.0.0/0")
    error_message = "Refusing 0.0.0.0/0: an unauthenticated dashboard open to the internet is remote code execution. Front it with an authenticating proxy instead."
  }
}

variable "preview_domain" {
  description = <<-EOT
    Base domain previews are served under, e.g. "preview.example.com".
    Previews resolve as <sha>-<repo>.<preview_domain> — one label, so a
    single wildcard record covers every repo.
  EOT
  type        = string
}

variable "route53_zone_id" {
  description = "Hosted zone for the preview records. Leave null to manage DNS elsewhere and point it at the public_ip output."
  type        = string
  default     = null
}

variable "subnet_id" {
  description = "Public subnet for the instance. Defaults to a subnet of the region's default VPC."
  type        = string
  default     = null
}

variable "enable_tls" {
  description = <<-EOT
    Front the server with an ALB holding an ACM certificate for
    <preview_domain> and *.<preview_domain>, and serve HTTPS. Requires
    route53_zone_id, since the certificate is DNS-validated.

    The server itself only speaks HTTP, so this is the only way to get TLS.
    With it on, the instance stops accepting traffic from anywhere but the
    load balancer.
  EOT
  type        = bool
  default     = true
}

variable "alb_subnet_ids" {
  description = "Subnets for the load balancer; needs at least two in different AZs. Defaults to the default VPC's."
  type        = list(string)
  default     = null
}

variable "oidc" {
  description = <<-EOT
    Authenticate browser sessions at the load balancer. This is what lets you
    widen allowed_ingress_cidrs beyond a trusted range, because the server
    itself has no authentication.

    Non-browser clients cannot complete an OIDC redirect, so the `preview`
    CLI and any webhook sender must come from oidc_bypass_cidrs.
  EOT
  type = object({
    issuer                 = string
    authorization_endpoint = string
    token_endpoint         = string
    user_info_endpoint     = string
    client_id              = string
    client_secret          = string
    scope                  = optional(string, "openid email")
    session_timeout        = optional(number, 43200)
  })
  default   = null
  sensitive = true
}

variable "oidc_bypass_cidrs" {
  description = "Source ranges that skip OIDC — for the CLI and webhook senders, which can't follow a login redirect. Still bounded by allowed_ingress_cidrs."
  type        = list(string)
  default     = []
}

variable "instance_type" {
  description = "Instance type. Previews build and run target repos, so size for the heaviest target build."
  type        = string
  default     = "t3.large"
}

variable "ami_id" {
  description = "AMI to boot. Defaults to the newest Amazon Linux 2023 for the instance's architecture."
  type        = string
  default     = null
}

variable "key_pair_name" {
  description = "EC2 key pair for SSH. Null relies on SSM Session Manager, which the instance profile already allows."
  type        = string
  default     = null
}

variable "allow_ssh" {
  description = "Open port 22 to allowed_ingress_cidrs. Off by default — the instance profile grants SSM Session Manager access."
  type        = bool
  default     = false
}

variable "http_port" {
  description = "Port the server listens on. Previews are addressed without an explicit port only when this is 80."
  type        = number
  default     = 80
}

variable "image" {
  description = "Container image to run. Pin a release tag rather than tracking latest."
  type        = string
  default     = "lahmanja/local-preview:latest"
}

variable "data_dir" {
  description = <<-EOT
    Absolute path of the data directory, mounted from a dedicated EBS volume
    and bind-mounted into the container at the SAME path — build containers
    are started through the host daemon, which resolves bind sources against
    its own filesystem.
  EOT
  type        = string
  default     = "/var/lib/local-preview"
}

variable "data_volume_size_gb" {
  description = "Size of the data volume. Artifacts, mirror clones, and per-artifact state accumulate here until retention evicts them."
  type        = number
  default     = 100
}

variable "root_volume_size_gb" {
  description = "Size of the root volume (OS + container images)."
  type        = number
  default     = 30
}

variable "github_webhook_secret_ssm_parameter" {
  description = <<-EOT
    Name of a SecureString SSM parameter holding the GitHub webhook secret.
    Read on the instance at boot, never passed as a Terraform variable —
    a plain variable would land in state and in the process table.
  EOT
  type        = string
  default     = null
}

variable "extra_server_args" {
  description = "Additional `preview serve` flags, e.g. [\"--max-warm\", \"16\"]."
  type        = list(string)
  default     = []
}

variable "tags" {
  description = "Tags merged into every resource."
  type        = map(string)
  default     = {}
}
