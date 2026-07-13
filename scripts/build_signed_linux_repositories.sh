#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 PACKAGE_DIR REPOSITORY_DIR OUTPUT_TAR_GZ" >&2
  exit 2
fi

package_dir=$(cd -- "$1" && pwd -P)
repository_name=$(basename -- "$2")
repository_parent=$(cd -- "$(dirname -- "$2")" && pwd -P)
output_name=$(basename -- "$3")
output_parent=$(cd -- "$(dirname -- "$3")" && pwd -P)
if [[ "$package_dir" == / || "$repository_parent" != "$package_dir" || \
      "$output_parent" != "$package_dir" ]]; then
  echo "repository and archive must be distinct direct children of PACKAGE_DIR" >&2
  exit 1
fi
if [[ -z "$repository_name" || "$repository_name" == . || "$repository_name" == .. || \
      -z "$output_name" || "$output_name" == . || "$output_name" == .. ]]; then
  echo "repository and archive names must be non-empty path components" >&2
  exit 1
fi
if [[ "$output_name" != *.tar.gz ]]; then
  echo "repository archive must have a .tar.gz filename" >&2
  exit 1
fi
repository_dir="$repository_parent/$repository_name"
output_archive="$output_parent/$output_name"
public_key_output="$output_parent/burnban-release-signing-key.asc"
if [[ "$repository_dir" == "$package_dir" || "$repository_dir" == "$output_archive" || \
      "$repository_dir" == "$public_key_output" || "$output_archive" == "$public_key_output" || \
      -L "$repository_dir" || -L "$output_archive" || -L "$public_key_output" || \
      ( -e "$repository_dir" && ! -d "$repository_dir" ) || \
      ( -e "$output_archive" && ! -f "$output_archive" ) || \
      ( -e "$public_key_output" && ! -f "$public_key_output" ) ]]; then
  echo "repository/archive paths are unsafe, overlapping, or symlinked" >&2
  exit 1
fi
key_file=${BURNBAN_RELEASE_GPG_KEY_FILE:-}
fingerprint=${BURNBAN_RELEASE_GPG_FINGERPRINT:-}
source_epoch=${SOURCE_DATE_EPOCH:-}
expected_version=${BURNBAN_RELEASE_VERSION:-}

for tool in ar apt-ftparchive createrepo_c dpkg-deb dpkg-scanpackages gpg gzip rpm sha256sum tar; do
  command -v "$tool" >/dev/null || {
    echo "required release tool is missing: $tool" >&2
    exit 1
  }
done
if [[ -z "$key_file" || ! -f "$key_file" ]]; then
  echo "BURNBAN_RELEASE_GPG_KEY_FILE must name the protected release private key" >&2
  exit 1
fi
fingerprint=${fingerprint//[[:space:]]/}
fingerprint=${fingerprint^^}
if [[ ! "$fingerprint" =~ ^[0-9A-F]{40,64}$ ]]; then
  echo "BURNBAN_RELEASE_GPG_FINGERPRINT must be a full hexadecimal fingerprint" >&2
  exit 1
fi
if [[ ! "$source_epoch" =~ ^[0-9]{10,}$ ]]; then
  echo "SOURCE_DATE_EPOCH must be the tagged commit timestamp" >&2
  exit 1
fi
expected_version=${expected_version#v}
if [[ ! "$expected_version" =~ ^[0-9][0-9A-Za-z.+:~_-]{0,127}$ ]]; then
  echo "BURNBAN_RELEASE_VERSION must be the exact package version" >&2
  exit 1
fi
# nFPM maps GoReleaser's snapshot separator into each package manager's
# ordering-safe syntax. Stable dispatch tags are unchanged by both mappings.
expected_deb_version=${expected_version/-SNAPSHOT-/~SNAPSHOT-}
expected_rpm_version=${expected_version/-SNAPSHOT-/~SNAPSHOT_}

mapfile -d '' debs < <(find "$package_dir" -maxdepth 1 -type f -name '*.deb' -print0 | sort -z)
mapfile -d '' rpms < <(find "$package_dir" -maxdepth 1 -type f -name '*.rpm' -print0 | sort -z)
if [[ ${#debs[@]} -eq 0 || ${#rpms[@]} -eq 0 ]]; then
  echo "both signed .deb and .rpm packages are required" >&2
  exit 1
fi
if [[ ${#debs[@]} -ne 2 || ${#rpms[@]} -ne 2 ]]; then
  echo "exactly two signed .deb and two signed .rpm packages are required" >&2
  exit 1
fi
for architecture in amd64 arm64; do
  matches=0
  for package in "${debs[@]}"; do
    if [[ $(dpkg-deb --field "$package" Package) == burnban && \
          $(dpkg-deb --field "$package" Architecture) == "$architecture" && \
          $(dpkg-deb --field "$package" Version) == "$expected_deb_version" ]]; then
      matches=$((matches + 1))
    fi
  done
  if [[ $matches -ne 1 ]]; then
    echo "expected one burnban Debian package for $architecture" >&2
    exit 1
  fi
done
for architecture in x86_64 aarch64; do
  matches=0
  for package in "${rpms[@]}"; do
    if [[ $(rpm -qp --queryformat '%{NAME}' "$package") == burnban && \
          $(rpm -qp --queryformat '%{ARCH}' "$package") == "$architecture" && \
          $(rpm -qp --queryformat '%{VERSION}' "$package") == "$expected_rpm_version" ]]; then
      matches=$((matches + 1))
    fi
  done
  if [[ $matches -ne 1 ]]; then
    echo "expected one burnban RPM package for $architecture" >&2
    exit 1
  fi
done

work=$(mktemp -d)
passphrase_file=
cleanup() {
  rm -rf "$work"
}
trap cleanup EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 0700 "$GNUPGHOME"
actual=$(gpg --batch --show-keys --with-colons "$key_file" | awk -F: '$1=="fpr" {print toupper($10); exit}')
if [[ "$actual" != "$fingerprint" ]]; then
  echo "release key fingerprint does not match the protected expected fingerprint" >&2
  exit 1
fi
gpg --batch --import "$key_file" >/dev/null 2>&1
if [[ -n ${BURNBAN_RELEASE_GPG_PASSPHRASE:-} ]]; then
  passphrase_file="$work/passphrase"
  umask 077
  builtin printf '%s' "$BURNBAN_RELEASE_GPG_PASSPHRASE" >"$passphrase_file"
fi
gpg_args=(--batch --yes --pinentry-mode loopback --local-user "$fingerprint")
if [[ -n "$passphrase_file" ]]; then
  gpg_args+=(--passphrase-file "$passphrase_file")
fi

public_key="$work/burnban-release-signing-key.asc"
gpg --batch --armor --export "$fingerprint" >"$public_key"
test -s "$public_key"
verify_gnupg="$work/verify-gnupg"
mkdir -m 0700 "$verify_gnupg"
gpg --batch --homedir "$verify_gnupg" --import "$public_key" >/dev/null 2>&1

# Verify the package signatures before indexing them. nFPM's debsign payload is
# the exact concatenation of these three ar members.
for package in "${debs[@]}"; do
  payload="$work/deb-payload"
  signature="$work/deb-signature"
  control_member=$(ar t "$package" | awk '/^control\.tar/ {print; exit}')
  data_member=$(ar t "$package" | awk '/^data\.tar/ {print; exit}')
  test -n "$control_member"
  test -n "$data_member"
  ar p "$package" debian-binary >"$payload"
  ar p "$package" "$control_member" >>"$payload"
  ar p "$package" "$data_member" >>"$payload"
  ar p "$package" _gpgorigin >"$signature"
  status=$(gpg --batch --homedir "$verify_gnupg" --status-fd=1 \
    --verify "$signature" "$payload" 2>/dev/null) || {
    echo "Debian package signature verification failed: $(basename "$package")" >&2
    exit 1
  }
  awk -v want="$fingerprint" \
    '$2 == "VALIDSIG" && toupper($NF) == want { valid=1 } END { exit !valid }' \
    <<<"$status" || {
    echo "Debian package was not signed by the expected primary key: $(basename "$package")" >&2
    exit 1
  }
done

rpm_db="$work/rpmdb"
mkdir "$rpm_db"
rpm --dbpath "$rpm_db" --initdb
rpm --dbpath "$rpm_db" --import "$public_key"
for package in "${rpms[@]}"; do
  verification=$(rpm --dbpath "$rpm_db" --checksig "$package")
  if [[ "$verification" != *"signatures OK"* && "$verification" != *"digests signatures OK"* ]]; then
    echo "RPM package signature verification failed: $(basename "$package")" >&2
    exit 1
  fi
done

rm -rf -- "$repository_dir"
mkdir -p "$repository_dir/apt/pool/main/b/burnban" "$repository_dir/rpm/packages"
for package in "${debs[@]}"; do
  cp "$package" "$repository_dir/apt/pool/main/b/burnban/"
done
for package in "${rpms[@]}"; do
  cp "$package" "$repository_dir/rpm/packages/"
done
cp "$public_key" "$repository_dir/apt/burnban-release-signing-key.asc"
cp "$public_key" "$repository_dir/rpm/burnban-release-signing-key.asc"

for architecture in amd64 arm64; do
  index_dir="$repository_dir/apt/dists/stable/main/binary-$architecture"
  mkdir -p "$index_dir"
  (
    cd "$repository_dir/apt"
    dpkg-scanpackages -a "$architecture" pool/main/b/burnban /dev/null >"dists/stable/main/binary-$architecture/Packages"
  )
  gzip -n -9 -c "$index_dir/Packages" >"$index_dir/Packages.gz"
done

release_date=$(date -u -d "@$source_epoch" -R)
(
  cd "$repository_dir/apt"
  apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=Burnban \
    -o APT::FTPArchive::Release::Label=Burnban \
    -o APT::FTPArchive::Release::Suite=stable \
    -o APT::FTPArchive::Release::Codename=stable \
    -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
    -o APT::FTPArchive::Release::Components=main \
    -o APT::FTPArchive::Release::Description="Burnban signed packages" \
    -o APT::FTPArchive::Release::Date="$release_date" \
    release dists/stable >dists/stable/Release
  gpg "${gpg_args[@]}" --armor --detach-sign --output dists/stable/Release.gpg dists/stable/Release
  gpg "${gpg_args[@]}" --armor --clearsign --output dists/stable/InRelease dists/stable/Release
)

createrepo_c --revision "$source_epoch" --set-timestamp-to-revision "$repository_dir/rpm" >/dev/null
gpg "${gpg_args[@]}" --armor --detach-sign \
  --output "$repository_dir/rpm/repodata/repomd.xml.asc" "$repository_dir/rpm/repodata/repomd.xml"

(
  cd "$repository_dir"
  find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >"$work/repository-SHA256SUMS"
  mv "$work/repository-SHA256SUMS" SHA256SUMS
  sha256sum --check SHA256SUMS >/dev/null
)

tar --sort=name --mtime="@$source_epoch" --owner=0 --group=0 --numeric-owner \
  -C "$repository_dir" -cf - . | gzip -n -9 >"$output_archive"
test -s "$output_archive"
cp "$public_key" "$public_key_output"
