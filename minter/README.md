# Minter

Minter is a HTTP server meant to be run on Kubernetes that returns the service account
token for the service account (if any) attached to the Kubernetes Pod the server is
running on.

Meant to be used to access UIs on Kubernetes (like Headlamp) that current require
logging in with a service account or OIDC credentials of the cluster administrator.

Given this just gives the identity token of the service account, this server should
only be accessible to the VPN network that only trusted individuals have access to!

