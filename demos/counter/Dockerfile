# The counter writes to / and /home/counter, so it needs a base image with a
# real root-owned filesystem; a FROM scratch image has no "/" entry.
FROM public.ecr.aws/docker/library/busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
COPY counter /ko-app/counter
ENTRYPOINT ["/ko-app/counter"]
