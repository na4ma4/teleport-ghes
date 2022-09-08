// Copyright 2022 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
use std::ffi::{CString, NulError};

/// WindowsPath is a String that we assume to be in the form
/// of a traditional DOS path:
///
/// https://docs.microsoft.com/en-us/dotnet/standard/io/file-path-formats
///
/// Because RDP device redirection is limited in the paths it uses, we can
/// further assume that it is in one of the following forms:
///
/// r"\Program Files\Custom Utilities\StringFinder.exe": An absolute path from the root of the current drive.
///
/// r"2018\January.xlsx": A relative path to a file in a subdirectory of the current directory.
#[derive(Debug, Clone)]
pub struct WindowsPath {
    pub path: String,
}

impl WindowsPath {
    pub fn len(&self) -> u32 {
        self.path.len() as u32
    }
}

impl From<String> for WindowsPath {
    fn from(path: String) -> WindowsPath {
        Self { path }
    }
}

/// UnixPath is a String that we assume to be in the form of a
/// Unix Path, qualified by the qualifications laid out in RFD 0067
///
/// https://github.com/gravitational/teleport/blob/master/rfd/0067-desktop-access-file-system-sharing.md
#[derive(Debug, Clone)]
pub struct UnixPath {
    pub path: String,
}

impl UnixPath {
    /// This function will create a CString from a UnixPath.
    ///
    /// # Errors
    ///
    /// This function will return an error if the UnixPath contains
    /// any characters that can't be handled by CString::new().
    pub fn to_cstring(&self) -> Result<CString, NulError> {
        CString::new(self.path.clone())
    }

    pub fn len(&self) -> u32 {
        self.path.len() as u32
    }

    pub fn last(&self) -> Option<&str> {
        self.path.split('/').last()
    }
}

impl From<&WindowsPath> for UnixPath {
    fn from(p: &WindowsPath) -> UnixPath {
        Self::from(to_unix_path(&p.path))
    }
}

impl From<String> for UnixPath {
    fn from(path: String) -> UnixPath {
        Self { path }
    }
}

/// Converts a String from the type of path that's sent to us by RDP
/// into a unix-style path, as specified in Teleport RFD 0067:
///
/// https://github.com/gravitational/teleport/blob/master/rfd/0067-desktop-access-file-system-sharing.md
fn to_unix_path(rdp_path: &str) -> String {
    // Convert r"\" to "/"
    let mut cleaned = rdp_path.replace('\\', "/");

    // If the string started with r"\", just remove it
    if cleaned.starts_with('/') {
        crop_first_n_letters(&mut cleaned, 1);
    }

    cleaned
}

/// Crops the first n letters off of a String (in-place).
fn crop_first_n_letters(s: &mut String, n: usize) {
    match s.char_indices().nth(n) {
        Some((pos, _)) => {
            s.drain(..pos);
        }
        None => {
            s.clear();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_to_unix_path() {
        assert_eq!(to_unix_path(r"\"), "");
        assert_eq!(to_unix_path(r"\desktop.ini"), "desktop.ini");
        assert_eq!(
            to_unix_path(r"\test_directory\desktop.ini"),
            "test_directory/desktop.ini"
        );
    }
}
