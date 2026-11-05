# The probe actor: Substrate's e2e probe (internal/e2e/fixtures/probe), built
# from the release source by deploy.sh. Like the counter, it needs a base
# image with a real root filesystem.
FROM public.ecr.aws/docker/library/busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
COPY probe /ko-app/probe
ENTRYPOINT ["/ko-app/probe"]
