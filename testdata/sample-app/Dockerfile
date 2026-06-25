# Smallest possible app for the dev-loop validation scenarios. Serves a
# single line of text whose content the four scenarios edit + commit to
# prove commit-to-pod propagation.
FROM busybox:1.37
COPY index.html /www/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
