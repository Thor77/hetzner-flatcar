passwd:
  users:
    - name: core
      ssh_authorized_keys:
        - {{ .SSHKey.PublicKey -}}
storage:
  files:
    - path: /etc/hostname
      filesystem: root
      contents:
        inline: {{ .Server.Name }}
