FROM planet/os

ARG SECCOMP_VER

# FIXME: allowing downgrades and pinning the version of libip4tc0 for iptables
# as the package has a dependency on the older version as the one available.
RUN export DEBIAN_FRONTEND=noninteractive && set -e && \
	apt-get update && apt-get install -q -y --allow-downgrades --no-install-recommends \
	bridge-utils \
        seccomp=$SECCOMP_VER \
        bash-completion \
        kmod \
        libip4tc0=1.6.0+snapshot20161117-6 \
        iptables \
        ebtables \
        libsqlite3-0 \
        e2fsprogs \
        libncurses5 \
        net-tools \
        curl \
        iproute2 \
        lsb-base \
        dash \
        ca-certificates \
        aufs-tools \
        xfsprogs \
        dbus \
        dnsutils \
        ethtool \
        sysstat \
        nano \
        vim \
        iotop \
        htop \
        ifstat \
        iftop \
        traceroute \
        tcpdump \
        procps \
        coreutils \
        lsof \
        socat \
        nmap \
        netcat \
        nfs-common \
        jq \
        conntrack \
        strace \
	&& apt-get --purge remove exim4 exim4-base exim4-config exim4-daemon-light \
	&& apt-get -y autoclean && apt-get -y clean && apt-get autoremove \
	&& rm -rf /var/lib/apt/lists/*;

RUN groupadd --system --non-unique --gid 1000 planet; \
    useradd --system --non-unique --no-create-home -g 1000 -u 1000 planet; \
    groupadd --system docker; \
    usermod -a -G planet root; \
    usermod -a -G docker planet;
