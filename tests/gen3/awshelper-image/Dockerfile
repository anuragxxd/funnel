FROM quay.io/cdis/awshelper@sha256:0b90890428340fbdb6657e1f2e599f623bf76eaaffa4f31fc8a4d75e98b5500c
USER root
ARG TARGETARCH
RUN test "$TARGETARCH" = arm64 && \
    curl --fail --location https://awscli.amazonaws.com/awscli-exe-linux-aarch64-2.31.35.zip -o /tmp/aws.zip && \
    unzip -q /tmp/aws.zip -d /tmp/aws-native && \
    /tmp/aws-native/aws/install --update && \
    rm -rf /tmp/aws.zip /tmp/aws-native
USER ubuntu
RUN aws --version && kubectl version --client
