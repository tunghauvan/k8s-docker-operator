# Docker mTLS Setup Guide

This guide explains how to secure a remote Docker daemon using mutual TLS (mTLS) and how to configure the Kubernetes Docker Operator to connect to it.

## 1. Generate Certificates

You can use the following script to generate the CA, server, and client certificates. Replace `<HOST_IP>` with the public/accessible IP of your Docker host.

### Script: `gen-certs.sh`
```bash
#!/bin/bash
set -e

HOST="<HOST_IP>"
PASSWORD="your-strong-password"

# 1. Generate CA
openssl genrsa -aes256 -passout pass:$PASSWORD -out ca-key.pem 4096
openssl req -new -x509 -days 365 -key ca-key.pem -sha256 -out ca.pem -passin pass:$PASSWORD -subj "/C=US/ST=State/L=City/O=Org/CN=$HOST"

# 2. Generate Server Key & Cert
openssl genrsa -out server-key.pem 4096
openssl req -subj "/CN=$HOST" -sha256 -new -key server-key.pem -out server.csr
echo "subjectAltName = IP:$HOST,IP:127.0.0.1" > extfile.cnf
echo "extendedKeyUsage = serverAuth" >> extfile.cnf
openssl x509 -req -days 365 -sha256 -in server.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out server-cert.pem -extfile extfile.cnf -passin pass:$PASSWORD

# 3. Generate Client Key & Cert
openssl genrsa -out key.pem 4096
openssl req -subj '/CN=client' -new -key key.pem -out client.csr
echo "extendedKeyUsage = clientAuth" > extfile-client.cnf
openssl x509 -req -days 365 -sha256 -in client.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out cert.pem -extfile extfile-client.cnf -passin pass:$PASSWORD

# 4. Cleanup
rm -f client.csr server.csr extfile.cnf extfile-client.cnf ca.srl
chmod 0400 ca-key.pem key.pem server-key.pem
chmod 0444 ca.pem server-cert.pem cert.pem
```

## 2. Configure Docker Daemon

On the target Docker host, move the server certificates to `/etc/docker/` and update the daemon configuration.

1. Copy files:
   ```bash
   cp ca.pem server-cert.pem server-key.pem /etc/docker/
   ```

2. Edit `/etc/docker/daemon.json`:
   ```json
   {
     "tlsverify": true,
     "tlscacert": "/etc/docker/ca.pem",
     "tlscert": "/etc/docker/server-cert.pem",
     "tlskey": "/etc/docker/server-key.pem",
     "hosts": ["tcp://0.0.0.0:2376", "unix:///var/run/docker.sock"]
   }
   ```

3. Restart Docker:
   ```bash
   systemctl restart docker
   ```

## 3. Create Kubernetes Secret

The operator expects a secret containing `ca.pem`, `cert.pem`, and `key.pem` (the client files).

```bash
kubectl create secret generic docker-remote-tls \
  --from-file=ca.pem=ca.pem \
  --from-file=cert.pem=cert.pem \
  --from-file=key.pem=key.pem
```

## 4. Reference in DockerHost

Now you can create a `DockerHost` resource pointing to your remote daemon:

```yaml
apiVersion: kdop.io.vn/v1alpha1
kind: DockerHost
metadata:
  name: remote-host
spec:
  hostURL: "tcp://<HOST_IP>:2376"
  tlsSecretName: docker-remote-tls
```
