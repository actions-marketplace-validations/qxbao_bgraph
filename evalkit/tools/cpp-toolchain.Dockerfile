FROM swift@sha256:0e4716bd34384d22963a63afbdbc93be3129dfd0753185aa1ded27755abdcae8

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential git lcov libgtest-dev pkg-config python3-venv valgrind && \
    rm -rf /var/lib/apt/lists/*
RUN python3 -m venv /opt/cpp-tools && /opt/cpp-tools/bin/pip install --no-cache-dir \
    cmake==3.31.10 meson==1.8.5 ninja==1.13.0

ENV PATH="/opt/cpp-tools/bin:${PATH}"
