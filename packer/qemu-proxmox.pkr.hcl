# Packer Template — QEMU/KVM & Proxmox VE Golden Image (qcow2)
# Cách chạy:
#   packer init packer/qemu-proxmox.pkr.hcl
#   packer build -var "bundle_path=bundles/vkube-bundle-v1.36.7-vkube.1-linux-amd64.tar.zst" packer/qemu-proxmox.pkr.hcl

packer {
  required_plugins {
    qemu = {
      version = ">= 1.1.0"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

variable "cloud_image_url" {
  type        = string
  description = "URL/đường dẫn Ubuntu 24.04 Cloud Image (.img/.qcow2)"
  default     = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
}

variable "cloud_image_checksum" {
  type    = string
  default = "none"
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

source "qemu" "vkube-ubuntu" {
  vm_name          = "vkube-golden-${var.release_tag}.qcow2"
  iso_url          = var.cloud_image_url
  iso_checksum     = var.cloud_image_checksum
  disk_image       = true
  disk_size        = "20G"
  format           = "qcow2"
  accelerator      = "kvm"
  cpus             = 2
  memory           = 2048
  headless         = true
  ssh_username     = var.ssh_username
  ssh_password     = var.ssh_password
  ssh_timeout      = "15m"
  output_directory = "output-vkube-qemu"
  shutdown_command = "echo '${var.ssh_password}' | sudo -S shutdown -P now"
}

build {
  sources = ["source.qemu.vkube-ubuntu"]

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
