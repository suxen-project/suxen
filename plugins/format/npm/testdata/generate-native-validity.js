// Regenerate with a Node installation containing semver 7.8.5:
// NODE_PATH=/path/to/node_modules node testdata/generate-native-validity.js \
//   > testdata/npm-semver-7.8.5-validity.tsv
const semver = require('semver')
const version = require('semver/package.json').version
if (version !== '7.8.5') {
  throw new Error(`expected semver 7.8.5, got ${version}`)
}
const versions = [
  '0.0.0',
  '9007199254740991.0.0',
  '0.9007199254740991.0',
  '0.0.9007199254740991',
  '9007199254740992.0.0',
  '0.9007199254740992.0',
  '0.0.9007199254740992',
  '1.0.0-' + '9'.repeat(40),
  '1.0.0+' + 'a'.repeat(250), // exactly 256 characters
  '1.0.0+' + 'a'.repeat(251),
  '1.2.3-alpha.1+build',
  '01.2.3',
]
for (const candidate of versions) {
  process.stdout.write(`${candidate}\t${semver.valid(candidate) !== null}\n`)
}
