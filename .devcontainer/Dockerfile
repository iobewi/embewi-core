FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV TZ=Europe/Paris

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
    gettext-base \
    git \
    iproute2 \
    locales \
    openssh-client \
    ovmf \
    python3 \
    python3-pip \
    rsync \
    sudo \
    unzip \
    wget \
    vim \
    npm \
    golang-go \
 && sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen \
 && locale-gen \
 && update-locale LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8 \
 && rm -rf /var/lib/apt/lists/*

# Documentation Sphinx (docs/) — mêmes contraintes que docs/requirements.txt
RUN pip3 install --no-cache-dir \
    "sphinx>=7.2,<9" \
    "myst-parser>=2.0,<5" \
    "sphinx-rtd-theme>=3.0" \
    "sphinxcontrib-mermaid>=0.9"
ENV LANG=en_US.UTF-8
ENV LANGUAGE=en_US:en
ENV LC_ALL=en_US.UTF-8

ENV HOME=/root

# Installer Claude Code via npm (le plus fiable en Docker)
RUN npm install -g @anthropic-ai/claude-code

# Configurer le PATH pour les shells interactifs
RUN echo 'export PATH="/root/.local/bin:$PATH"' >> /root/.bashrc

CMD ["/bin/bash"]