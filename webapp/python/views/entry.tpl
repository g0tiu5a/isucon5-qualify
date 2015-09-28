% rebase("layout")
<h2>{{owner["nick_name"]}}さんの日記</h2>
<div class="row panel panel-primary" id="entry-entry">
  <div class="entry-title">タイトル: <a href="/diary/entry/{{entry["id"]}}">{{entry["title"]}}</a></div>
  <div class="entry-content">
    % for line in entry["content"].split("\n"):
      {{line}}<br />
    % end
  </div>
  % if entry["is_private"]:
    <div class="entry-private">範囲: 友だち限定公開</div>
  % end
  <div class="entry-created-at">更新日時: {{entry["created_at"]}}</div>
</div>

<h3>この日記へのコメント</h3>
<div class="row panel panel-primary" id="entry-comments">
  % for comment in comments:
    <div class="comment">
      % comment_user = get_user(comment["user_id"])
      <div class="comment-owner"><a href="/profile/{{comment_user["account_name"]}}">{{comment_user["nick_name"]}}さん</a></div>
      <div class="comment-comment">
        % for line in comment["comment"].split("\n"):
          {{line}}<br />
        % end
      </div>
      <div class="comment-created-at">投稿時刻:{{comment["created_at"]}}</div>
    </div>
  % end
</div>

<h3>コメントを投稿</h3>
<div id="entry-comment-form">
  <form method="POST" action="/diary/comment/{{entry["id"]}}">
    <div>コメント: <textarea name="comment"></textarea></div>
    <div><input type="submit" value="送信" /></div>
  </form>
</div>
