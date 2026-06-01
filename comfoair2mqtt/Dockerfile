ARG BUILD_ARCH
FROM ghcr.io/home-assistant/${BUILD_ARCH}-base-python:3.10-alpine3.14

# Upstream is pinned to a verified-compatible commit. The clone used to be
# unpinned, so every image rebuild silently pulled the latest upstream HEAD. When
# upstream added the mandatory [DEVICE] config section (fan levels) the generated
# config.ini no longer matched its contract and ca350.py crashed at startup.
# Bump this SHA deliberately after verifying the entrypoint still generates a
# matching config.ini. paho-mqtt is pinned to 2.x because the pinned upstream uses
# the v2 callback API (mqtt.CallbackAPIVersion.VERSION2).
ARG HACOMFOAIRMQTT_REF=2d0da1024a58e3e21a565dfa78b9034a476a3ab2
RUN apk add git bash \
 && cd /opt \
 && git clone https://github.com/adorobis/hacomfoairmqtt.git \
 && git -C hacomfoairmqtt checkout "${HACOMFOAIRMQTT_REF}" \
 && python3 -m venv /opt/hacomfoairmqtt \
 && cd /opt/hacomfoairmqtt \
 && pip3 install 'paho-mqtt>=2.1,<3' pyserial pyyaml \
 && source /opt/hacomfoairmqtt/bin/activate \
 && rm /var/cache/apk/* \
 && rm -rf /root/.cache /root/.npm \
 && cp /opt/hacomfoairmqtt/src/ca350.py /opt/hacomfoairmqtt/src/ca350.original \
 && sed -i 's|/usr/local/share/ca350/bin/python3.8|/opt/hacomfoairmqtt/bin/python3|g' /opt/hacomfoairmqtt/src/ca350.py

WORKDIR /opt/hacomfoairmqtt

COPY rootfs /
RUN chmod +x /*.sh
ENTRYPOINT ["/docker-entrypoint.sh"]
