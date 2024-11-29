# r2sync - R2 Storage Synchronization Tool

A command-line tool for synchronizing local directories with Cloudflare R2 Storage.

## Features

- Upload files to R2 Storage
- Delete remote files that don't exist locally (optional)
- Dry-run mode to preview changes
- Recursive directory synchronization
- Concurrent file operations
- Progress and speed reporting

## Config

### Use the shared AWS config and credentials files

(Location of the shared config and credentials files)[https://docs.aws.amazon.com/sdkref/latest/guide/file-location.html]

By default, the files are in a folder named .aws that is placed in your home or user folder.

| Operating system | Default location and name of files                           |
| ---------------- | ------------------------------------------------------------ |
| Linux and macOS  | *~/.aws/config* *~/.aws/credentials*                         |
| Windows          | *%USERPROFILE%/.aws/config* *%USERPROFILE%/.aws/credentials* |

**`~/.aws/config`**
```conf
[default]
region = auto
endpoint_url = https://<ACCOUNT_ID>.r2.cloudflarestorage.com/
```

**`~/.aws/credentials`**
```conf
[default]
aws_access_key_id = <YOUR_AWS_ACCESS_KEY>
aws_secret_access_key = <YOUR_AWS_SECRET_ACCESS_KEY>
```

## Install

```bash
sudo curl -L "https://github.com/gofika/r2sync/releases/latest/download/r2sync-$(uname -s)-$(uname -m)" -o /usr/local/bin/r2sync
sudo chmod +x /usr/local/bin/r2sync
```


## Usage

```bash
r2sync [--dryrun] [--delete] [--recursive] <source path> <target path>
```

### Options

- `--dryrun`: Preview operations without executing them
- `--delete`: Remove files from R2 that don't exist in the source
- `--recursive`: Synchronize subdirectories recursively
- `--concurrency N`: Number of concurrent upload/delete operations (default: 5)
- `--exclude PATTERN`: Exclude file or directory patterns (can be used multiple times)
- `--size-only`: Only use file size to determine if files are the same

### Target Path Format

The target path should be in the format: `r2://bucket-name/optional/path/`

## Examples

1. Basic sync from local directory to R2:

```bash
r2sync ./local/photos r2://my-bucket/photos/
```


2. Preview sync operations without executing:

```bash
r2sync /local/docs r2://my-bucket/documents/
```


3. Sync with deletion of removed files:

```bash
r2sync --delete /website r2://my-bucket/public/
```


4. Full recursive sync with deletion and preview:

```bash
r2sync --recursive --delete /content r2://my-bucket/data/
```


5. Sync with custom concurrency and exclusions:

```bash
r2sync --recursive --delete --concurrency 10 --exclude '*.tmp' --exclude 'backup/*' /content r2://my-bucket/data/
```


## Notes

- The tool uses AWS SDK credentials configuration
- Files are compared using size and MD5 hash (unless --size-only is specified)
- MIME types are automatically detected based on file extensions
- Concurrent operations are configurable (default: 5 simultaneous transfers)
- Progress and transfer speeds are displayed during operations