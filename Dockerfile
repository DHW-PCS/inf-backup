FROM --platform=$TARGETOS/$TARGETARCH alpine:latest

LABEL author="DHW PCS Maintainers" maintainer="maintainers@dhw.one"

LABEL org.opencontainers.image.source="https://github.com/DHW-PCS/inf-backup"

RUN apk add --update --no-cache ca-certificates tzdata python3-dev py3-pip build-base unzip restic rclone 
RUN adduser -D -h /home/container container

COPY ./app /app
RUN chown -R container:container /app
RUN cat /app/requirements.txt
RUN pip3 install --no-cache-dir --break-system-packages -r /app/requirements.txt
USER container
ENV USER=container HOME=/home/container
WORKDIR /home/container
COPY ./entrypoint-posix.sh /entrypoint.sh
CMD [ "/bin/ash", "/entrypoint.sh" ]