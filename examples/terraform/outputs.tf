output "public_ip" {
  description = "Elastic IP of the server. Point DNS here when route53_zone_id isn't set."
  value       = aws_eip.server.public_ip
}

output "instance_id" {
  description = "EC2 instance id."
  value       = aws_instance.server.id
}

output "dashboard_url" {
  description = "Dashboard URL. Only resolves once the preview_domain record exists."
  value       = var.http_port == 80 ? "http://${var.preview_domain}/" : "http://${var.preview_domain}:${var.http_port}/"
}

output "session_command" {
  description = "Open a shell on the instance without SSH."
  value       = "aws ssm start-session --region ${data.aws_region.current.name} --target ${aws_instance.server.id}"
}

output "dns_records" {
  description = "Records that must exist, for zones this module doesn't manage."
  value = [
    { name = var.preview_domain, type = "A", value = aws_eip.server.public_ip },
    { name = "*.${var.preview_domain}", type = "A", value = aws_eip.server.public_ip },
  ]
}
