BINARY   = embewi-core
IMG      ?= embewi/core:latest

.PHONY: build tidy generate manifests docker-build docker-push install uninstall deploy registry

build:
	go build -o bin/$(BINARY) ./cmd/controller/

tidy:
	go mod tidy

# Génère le DeepCopy (nécessite controller-gen).
generate:
	controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

# Génère les CRDs YAML depuis les markers kubebuilder.
manifests:
	controller-gen crd paths="./..." output:crd:artifacts:config=config/crd/bases

docker-build:
	docker build -t $(IMG) .

docker-push: docker-build
	docker push $(IMG)

# Installe les CRDs + RBAC sur le cluster courant.
install:
	kubectl apply -f config/crd/bases/
	kubectl apply -f config/rbac/

# Déploie le controller (namespace + Deployment + Service heartbeat).
deploy: install
	kubectl apply -f config/manager/deployment.yaml

# Supprime le controller et le RBAC (conserve les CRDs et les ressources utilisateur).
uninstall:
	kubectl delete -f config/manager/deployment.yaml --ignore-not-found
	kubectl delete -f config/rbac/ --ignore-not-found

# Déploie le registre OCI Zot in-cluster.
registry:
	kubectl apply -f config/registry/zot.yaml
