# filematch

Due to the Docker/Moby renaming (?) it wasn't importing correctly, so to get this to work, it was copy/pasted from Docker/Moby fileutils `"github.com/docker/docker/pkg/fileutils"`. As a side benefit, all the non-matching (file) code was removed.

This is used in `.dockerignore` file parsing.
