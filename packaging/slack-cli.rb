# Homebrew formula。tap リポジトリ (jiikko/homebrew-tap) の Formula/ へ置く正本のコピー。
#
# 🚨 このファイルを更新したら tap 側にも反映すること（tap に無いと
# `brew install jiikko/tap/slack-cli` は届かない）。sha256 はリリース tarball のもの:
#   curl -sL https://github.com/jiikko/slack-cli/archive/refs/tags/vX.Y.Z.tar.gz | shasum -a 256
class SlackCli < Formula
  desc "Read-only CLI for Slack that borrows your Chrome login session"
  homepage "https://github.com/jiikko/slack-cli"
  url "https://github.com/jiikko/slack-cli/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "a05aadabca012310cbff35cd5b5570aca3691b48d4de893f7b1a39458972885b"
  license "MIT"
  head "https://github.com/jiikko/slack-cli.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w"), "./cmd/slack"
  end

  test do
    assert_match "slack - ", shell_output("#{bin}/slack --help")
    # 引数不足は終了コード 2（使い方エラー）
    output = shell_output("#{bin}/slack history 2>&1", 2)
    assert_match "チャンネルを指定してください", output
  end
end
