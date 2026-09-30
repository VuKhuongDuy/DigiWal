// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// Minimal ERC-20 for the e2e test only: the deployer is minted the whole supply and
// transfers move balances and emit Transfer. No allowance logic — nothing here uses it.
contract MockERC20 {
    string public constant name = "Test Token";
    string public constant symbol = "TT";
    uint8 public constant decimals = 18;
    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;

    event Transfer(address indexed from, address indexed to, uint256 value);

    constructor(uint256 supply) {
        totalSupply = supply;
        balanceOf[msg.sender] = supply;
        emit Transfer(address(0), msg.sender, supply);
    }

    function transfer(address to, uint256 value) external returns (bool) {
        require(balanceOf[msg.sender] >= value, "insufficient balance");
        balanceOf[msg.sender] -= value;
        balanceOf[to] += value;
        emit Transfer(msg.sender, to, value);
        return true;
    }
}
