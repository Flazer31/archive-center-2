// Exercises the installer with real ZIP extraction and SHA-256, without network
// or package-manager access. The fixture launcher records the inherited stdin.
const assert = require('node:assert/strict');
const {spawnSync} = require('node:child_process');
const {createHash} = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const bash = process.env.AC2_TEST_BASH || (process.platform === 'win32'
  ? 'C:/Program Files/Git/bin/bash.exe' : '/bin/bash');
const root = fs.mkdtempSync(path.join(path.resolve(__dirname, '..'), '.install-test-'));
const version = 'ac2-beta-0.0.6';
const launch = '#!/bin/sh\nIFS= read -r input\nprintf "%s" "$input" > launched\n';
function command(program, args, options) {
  const result = spawnSync(program, args, {encoding: 'utf8', timeout: 15000, ...options});
  if (result.error) throw result.error;
  return result;
}

function fixture(platform, arch, prefix, outcome) {
  const dir = fs.mkdtempSync(path.join(root, 'case-'));
  const file = (name, content) => fs.writeFileSync(path.join(dir, name), content, {mode: 0o700});
  const asset = `${version}-${platform}.zip`;
  fs.mkdirSync(path.join(dir, 'fake'));
  fs.mkdirSync(path.join(dir, 'home/archive-center2/old'), {recursive: true});
  file('home/archive-center2/old/existing', 'preserved');
  file('home/shareddata', 'preserved');
  file('fake/uname', `#!/bin/sh
case "$1" in -s) echo Linux ;; -m) echo '${arch}' ;; *) exit 1 ;; esac
`);
  file('fake/curl', `#!/bin/sh
set -eu
[ "$1" = -fsSL ] && [ "$3" = -o ]
case "$2" in
  https://github.com/test-owner/test-repo/releases/download/${version}/SHA256SUMS.txt)
    cp fixture-sums "$4" ;;
  https://github.com/test-owner/test-repo/releases/download/${version}/${asset})
    cp fixture.zip "$4" ;;
  *) exit 98 ;;
esac
printf '%s\\n' "$2" >> downloads
`);
  const archive = command(process.env.AC2_TEST_PYTHON || (process.platform === 'win32' ? 'python' : 'python3'),
    ['-c', 'import sys,zipfile\nwith zipfile.ZipFile("fixture.zip", "w") as z: z.writestr("01_AC2_start.sh", sys.argv[1])',
      launch + (outcome === 'launch-error' ? 'exit 7\n' : '')],
    {cwd: dir});
  assert.equal(archive.status, 0, archive.stderr);
  const hash = createHash('sha256').update(fs.readFileSync(path.join(dir, 'fixture.zip'))).digest('hex');
  file('fixture-sums', `${outcome === 'checksum' ? '0'.repeat(64) : hash}  ${asset}\r\n`);
  if (outcome === 'exists') {
    fs.mkdirSync(path.join(dir, 'home/archive-center2', version));
    file(`home/archive-center2/${version}/01_AC2_start.sh`, 'existing version');
  }
  fs.copyFileSync(path.join(__dirname, '../install.sh'), path.join(dir, 'install.sh'));
  const result = command(bash,
    ['-c', 'PATH="$PWD/fake:$PATH"; export PATH; command -v unzip && command -v sha256sum || exit 97; exec sh ./install.sh --repo test-owner/test-repo --version "$1"', 'test', version],
    {cwd: dir, input: 'terminal input\n', env: {...process.env, HOME: 'home', PREFIX: prefix}});
  const installed = path.join(dir, 'home/archive-center2', version);
  const read = (name) => fs.readFileSync(path.join(dir, name), 'utf8');
  if (outcome === 'success' || outcome === 'launch-error') {
    assert.equal(result.status, outcome === 'success' ? 0 : 7, result.stdout + result.stderr);
    assert.equal(read('launched'), 'terminal input');
    assert.equal(read('downloads').trim().split('\n').length, 2);
  } else {
    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.equal(fs.existsSync(path.join(dir, 'launched')), false);
    if (outcome === 'checksum') {
      assert.match(result.stderr, /SHA-256/);
      assert.equal(fs.existsSync(installed), false);
    } else if (outcome === 'exists') {
      assert.equal(fs.existsSync(path.join(dir, 'downloads')), false);
      assert.equal(fs.readFileSync(path.join(installed, '01_AC2_start.sh'), 'utf8'), 'existing version');
    } else {
      assert.match(result.stderr, /Linux\/i686/);
      assert.equal(fs.existsSync(path.join(dir, 'downloads')), false);
      assert.equal(fs.existsSync(installed), false);
    }
  }
  assert.equal(read('home/shareddata'), 'preserved');
  assert.equal(read('home/archive-center2/old/existing'), 'preserved');
  assert.deepEqual(fs.readdirSync(path.join(dir, 'home')).sort(), ['archive-center2', 'shareddata']);
  console.log(`PASS ${platform} ${outcome}`);
}

try {
  fixture('android-arm64', 'aarch64', '/data/data/com.termux/files/usr', 'success');
  fixture('linux-amd64', 'x86_64', '', 'success');
  fixture('linux-arm64', 'aarch64', '', 'success');
  fixture('linux-amd64', 'x86_64', '', 'checksum');
  fixture('linux-amd64', 'x86_64', '', 'exists');
  fixture('linux-amd64', 'x86_64', '', 'launch-error');
  fixture('linux-amd64', 'i686', '', 'unsupported');
} finally {
  fs.rmSync(root, {recursive: true, force: true});
}
