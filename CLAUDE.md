# slack-cli

- **新しい版を公開したら（tag を push して homebrew-tap の formula を更新したら）、手元で
  `brew uninstall` → `brew install` → 疎通コマンドまで実行してから完了と報告する**。手順とコマンドは
  README の「リリース」節が正本（ここに写さない）。CI の緑・tag の push・tap の更新だけでは、
  利用者の環境で入って動くことは確かめられていない
- Chrome の Cookie の復号・一時コピーの後始末・プロファイルの列挙は
  `github.com/jiikko/dotfiles/src/chromecookie` が持つ。直すときはあちらを直し、
  `go get github.com/jiikko/dotfiles/src/chromecookie@master` で取り込み直す（ここにコピーを戻さない）
