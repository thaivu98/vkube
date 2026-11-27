# Packer Template — Ubuntu Golden Image cho VMware (Workstation / Fusion / vSphere)
# Cách chạy:
#   packer init packer/vmware-ubuntu.pkr.hcl
#   packer build -var "bundle_path=bundles/vkube-bundle-v1.36.7-vkube.1-linux-amd64.tar.zst" packer/vmware-ubuntu.pkr.hcl

packer {
  required_plugins {
    vmware = {
      version = ">= 1.0.11"
      source  = "github.com/hashicorp/vmware"
    }
  }
}

variable "iso_url" {
  type        = string
  description = "Đường dẫn hoặc URL ISO Ubuntu 24.04 LTS Server"
  default     = "https://releases.ubuntu.com/24.04/ubuntu-24.04.2-live-server-amd64.iso"
}

variable "iso_checksum" {
  type        = string
  description = "Checksum SHA256 của file ISO (ví dụ: sha256:...)"
  default     = "none"
}

variable "bundle_path" {
  type        = string
  description = "Đường dẫn tới Offline Bundle (.tar.zst) tạo bởi `vkube bundle build`"
}

variable "release_tag" {
  type    = string
  default = "v1.36.7-vkube.1"
}

variable "ssh_username" {
  type    = string
  default = "ubuntu"
}

variable "ssh_password" {
  type      = string
  sensitive = true
  default   = "ubuntu"
}

source "vmware-iso" "vkube-ubuntu" {
  vm_name          = "vkube-golden-${var.release_tag}"
  guest_os_type    = "ubuntu-64"
  iso_url          = var.iso_url
  iso_checksum     = var.iso_checksum
  cpus             = 2
  memory           = 2048
  disk_size        = 20480
  headless         = true
  ssh_username     = var.ssh_username
  ssh_password     = var.ssh_password
  ssh_timeout      = "20m"
  shutdown_command = "echo '${var.ssh_password}' | sudo -S shutdown -P now"
}

build {
  sources = ["source.vmware-iso.vkube-ubuntu"]

  provisioner "file" {
    source      = var.bundle_path
    destination = "/tmp/vkube-bundle.tar.zst"
  }

  provisioner "shell" {
    execute_command = "echo '${var.ssh_password}' | {{.Vars}} sudo -S -E bash '{{.Path}}'"
    environment_vars = [
      "VKUBE_BUNDLE_PATH=/tmp/vkube-bundle.tar.zst",
      "VKUBE_RELEASE_TAG=${var.release_tag}",
    ]
    script = "${path.root}/scripts/bake-vkube-node.sh"
  }
}
