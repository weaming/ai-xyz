use std::future::Future;
use std::io;
use std::path::PathBuf;
use std::pin::Pin;

use codex_utils_path_uri::PathUri;

pub type FileSystemFuture<'a, T> = Pin<Box<dyn Future<Output = io::Result<T>> + Send + 'a>>;

#[derive(Clone, Copy, Debug, Default)]
pub struct FileSystemSandboxContext;

#[derive(Clone, Copy, Debug, Default)]
pub struct ReadFileOptions {
    pub follow_symlinks: bool,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct WriteFileOptions {
    pub follow_symlinks: bool,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct CreateDirectoryOptions {
    pub recursive: bool,
    pub follow_symlinks: bool,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct GetMetadataOptions {
    pub follow_symlinks: bool,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct RemoveOptions {
    pub recursive: bool,
    pub force: bool,
    pub follow_symlinks: bool,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct FileMetadata {
    pub is_file: bool,
    pub is_directory: bool,
    pub is_symlink: bool,
}

pub trait ExecutorFileSystem: Send + Sync {
    fn read_file_text<'a>(
        &'a self,
        path: &'a PathUri,
        options: ReadFileOptions,
        sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, String>;

    fn write_file<'a>(
        &'a self,
        path: &'a PathUri,
        contents: Vec<u8>,
        options: WriteFileOptions,
        sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, ()>;

    fn create_directory<'a>(
        &'a self,
        path: &'a PathUri,
        options: CreateDirectoryOptions,
        sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, ()>;

    fn get_metadata<'a>(
        &'a self,
        path: &'a PathUri,
        options: GetMetadataOptions,
        sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, FileMetadata>;

    fn remove<'a>(
        &'a self,
        path: &'a PathUri,
        options: RemoveOptions,
        sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, ()>;
}

#[derive(Clone, Copy, Debug, Default)]
pub struct LocalFileSystem;

pub static LOCAL_FS: LocalFileSystem = LocalFileSystem;

impl AsRef<LocalFileSystem> for LocalFileSystem {
    fn as_ref(&self) -> &LocalFileSystem {
        self
    }
}

impl LocalFileSystem {
    fn path(path: &PathUri) -> io::Result<PathBuf> {
        Ok(path.to_abs_path()?.as_path().to_path_buf())
    }

    fn reject_symlink(path: &PathBuf, follow_symlinks: bool) -> io::Result<()> {
        if !follow_symlinks && std::fs::symlink_metadata(path)?.is_symlink() {
            return Err(io::Error::new(
                io::ErrorKind::PermissionDenied,
                "symlink traversal is disabled",
            ));
        }
        Ok(())
    }
}

impl ExecutorFileSystem for LocalFileSystem {
    fn read_file_text<'a>(
        &'a self,
        path: &'a PathUri,
        options: ReadFileOptions,
        _sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, String> {
        Box::pin(async move {
            let path = Self::path(path)?;
            Self::reject_symlink(&path, options.follow_symlinks)?;
            let contents = std::fs::read(path)?;
            String::from_utf8(contents)
                .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))
        })
    }

    fn write_file<'a>(
        &'a self,
        path: &'a PathUri,
        contents: Vec<u8>,
        options: WriteFileOptions,
        _sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, ()> {
        Box::pin(async move {
            let path = Self::path(path)?;
            if path.exists() {
                Self::reject_symlink(&path, options.follow_symlinks)?;
            }
            std::fs::write(path, contents)
        })
    }

    fn create_directory<'a>(
        &'a self,
        path: &'a PathUri,
        options: CreateDirectoryOptions,
        _sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, ()> {
        Box::pin(async move {
            let path = Self::path(path)?;
            if options.follow_symlinks {
                if options.recursive {
                    std::fs::create_dir_all(path)
                } else {
                    std::fs::create_dir(path)
                }
            } else {
                Self::reject_symlink(&path, false)?;
                if options.recursive {
                    std::fs::create_dir_all(path)
                } else {
                    std::fs::create_dir(path)
                }
            }
        })
    }

    fn get_metadata<'a>(
        &'a self,
        path: &'a PathUri,
        options: GetMetadataOptions,
        _sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, FileMetadata> {
        Box::pin(async move {
            let path = Self::path(path)?;
            let metadata = if options.follow_symlinks {
                std::fs::metadata(&path)?
            } else {
                std::fs::symlink_metadata(&path)?
            };
            Ok(FileMetadata {
                is_file: metadata.is_file(),
                is_directory: metadata.is_dir(),
                is_symlink: metadata.file_type().is_symlink(),
            })
        })
    }

    fn remove<'a>(
        &'a self,
        path: &'a PathUri,
        options: RemoveOptions,
        _sandbox: Option<&'a FileSystemSandboxContext>,
    ) -> FileSystemFuture<'a, ()> {
        Box::pin(async move {
            let path = Self::path(path)?;
            Self::reject_symlink(&path, options.follow_symlinks)?;
            let result = if options.recursive {
                std::fs::remove_dir_all(path)
            } else {
                std::fs::remove_file(path)
            };
            if options.force
                && result
                    .as_ref()
                    .is_err_and(|error| error.kind() == io::ErrorKind::NotFound)
            {
                return Ok(());
            }
            result
        })
    }
}
