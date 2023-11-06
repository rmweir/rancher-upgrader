# Rancher-Upgrader
Rancher upgrader informs users on rancher release, helps users upgrade, and enforces rancher's supported upgrade path.

Rancher-upgrader's main goal is to educate users on what they're signing up for when performing a certain upgrade. The actual upgrade is just a bonus.

## How to Use
`rancher-upgrader --kubeconfig=<kube-config-path> upgrade`

The "upgrade" command will provide the user with an interactive prompt that guides them through an upgrade and everything they need to know.
